package schemachange_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// The tests below run slot loss (ST-4) end to end against a real decoded
// stream: a run copies the workload table and catches up, its walsender is
// killed and its slot dropped from under it, the source moves while nothing
// captures it, and reconcile mode has to put the shadow back in step without
// recopying it. Row-by-row comparison of source and shadow is the oracle.

// reconcileFixture is one database on a cluster that decodes WAL, with the
// seeded workload table, its copy-and-swap proof, the checkpoint store, and
// the table lock a run holds throughout.
type reconcileFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	table  testutil.WorkloadTable
	target preflight.CopySwapTarget
	store  *checkpoint.Store
	lock   *dbconn.TableLockSession
}

func newReconcileFixture(t *testing.T, rows int) reconcileFixture {
	t.Helper()
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s")
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, serverURL), MaxConns: 16}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	table := testutil.NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), rows))
	target, err := preflight.CheckCopySwap(t.Context(), pool, table.Schema, table.Table,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)

	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	require.NoError(t, store.Ensure(t.Context()))

	lock, err := dbconn.AcquireTableLock(t.Context(), cfg, table.Schema, table.Table)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, lock.Release(context.WithoutCancel(t.Context()))) })
	return reconcileFixture{cfg: cfg, pool: pool, table: table, target: target, store: store, lock: lock}
}

// exec runs SQL with %s standing for the qualified workload table.
func (f reconcileFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(sql, f.table.Qualified()))
	require.NoError(t, err)
}

func (f reconcileFixture) currentWALLSN(t *testing.T) decode.LSN {
	t.Helper()
	var text string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_current_wal_lsn()::text`).Scan(&text))
	lsn, err := decode.ParseLSN(text)
	require.NoError(t, err)
	return lsn
}

// buildShadow builds the shadow every test here runs: the workload table
// loses its label column.
func (f reconcileFixture) buildShadow(t *testing.T) schemachange.BuiltShadow {
	t.Helper()
	alter, err := statement.ParseOne(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN label`, f.table.Qualified()))
	require.NoError(t, err)
	shadow, err := schemachange.BuildShadow(t.Context(), f.pool, f.lock, f.target, alter, schemachange.Options{})
	require.NoError(t, err)
	return shadow
}

// row is the checkpoint a run would save for shadow at watermark, in phase,
// decoding from slotName with applied as its last applied position.
func (f reconcileFixture) row(shadow schemachange.BuiltShadow, phase checkpoint.Phase, watermark copier.Watermark, slotName string, applied decode.LSN) checkpoint.Checkpoint {
	return checkpoint.Checkpoint{
		Schema:            f.target.Schema(),
		Table:             f.target.Table(),
		ShadowTable:       shadow.ShadowTable(),
		SlotName:          slotName,
		PublicationName:   slotName,
		Watermark:         watermark,
		LastAppliedLSN:    applied,
		SourceFingerprint: shadow.SourceFingerprint(),
		TargetFingerprint: shadow.TargetFingerprint(),
		Phase:             phase,
	}
}

func (f reconcileFixture) load(t *testing.T, shadow schemachange.BuiltShadow) checkpoint.Checkpoint {
	t.Helper()
	cp, err := f.store.Load(t.Context(), f.target.Schema(), f.target.Table(),
		checkpoint.Fingerprints{Source: shadow.SourceFingerprint(), Target: shadow.TargetFingerprint()})
	require.NoError(t, err)
	return cp
}

func (f reconcileFixture) assertConverged(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.table.Schema, Table: shadow.SourceTable()},
		testutil.RelationRef{Schema: f.table.Schema, Table: shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"label"}})
}

// capture is one catch-up over one stream: the consumer half of a run.
type capture struct {
	stream  *decode.Stream
	catchup *applier.Catchup
	cancel  context.CancelFunc
	done    chan error
}

// startCapture opens the stream at from and runs a catch-up over it with
// the copier as its judge of keys. The caller ends it with stop or by
// killing its walsender and reading the error from wait.
func (f reconcileFixture) startCapture(t *testing.T, shadow schemachange.BuiltShadow, c *copier.Copier, from decode.LSN) *capture {
	t.Helper()
	stream, err := decode.OpenStream(t.Context(), f.cfg, f.pool, f.target, from)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, stream.Close(context.WithoutCancel(t.Context()))) })
	flusher, err := applier.NewFlusher(f.target, shadow, f.lock, applier.Options{})
	require.NoError(t, err)
	catchup, err := applier.NewCatchup(stream, flusher, c, applier.CatchupOptions{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cap := &capture{stream: stream, catchup: catchup, cancel: cancel, done: make(chan error, 1)}
	go func() { cap.done <- catchup.Run(ctx, f.pool) }()
	t.Cleanup(func() {
		cancel()
		_ = cap.wait(t)
	})
	return cap
}

// awaitConfirmedPast polls until the catch-up has confirmed past end and
// measured the server's write position there: every change committed
// before end is then in the shadow.
func (c *capture) awaitConfirmedPast(t *testing.T, end decode.LSN) applier.Status {
	t.Helper()
	const deadline = 30 * time.Second
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case err := <-c.done:
			t.Fatalf("catch-up returned while the test still needs it running: %v", err)
		default:
		}
		s := c.catchup.Status()
		if s.Confirmed >= end && s.WALEnd >= end {
			return s
		}
		select {
		case <-timer.C:
			t.Fatalf("catch-up did not confirm past %s within %s; last status %+v", end, deadline, s)
		case <-poll.C:
		}
	}
}

func (c *capture) stop(t *testing.T) {
	t.Helper()
	c.cancel()
	require.ErrorIs(t, c.wait(t), context.Canceled)
}

func (c *capture) wait(t *testing.T) error {
	t.Helper()
	const deadline = 15 * time.Second
	select {
	case err := <-c.done:
		c.done <- err
		return err
	case <-time.After(deadline):
		t.Fatalf("catch-up did not return within %s", deadline)
		return nil
	}
}

// killWalsender terminates the backend holding the slot, the way a failover
// or an operator ends a walsender: the stream's receive fails with the
// server's admin-shutdown error and the catch-up returns it.
func (f reconcileFixture) killWalsender(t *testing.T, cap *capture, slotName string) {
	t.Helper()
	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = $1 AND active`, slotName).Scan(&terminated))
	require.True(t, terminated)
	err := cap.wait(t)
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "57P01", pgErr.Code, "the walsender was terminated by an administrator")
}

// copyAll runs the copier to completion while a capture consumes the
// stream, the way a run copies.
func copyAll(t *testing.T, f reconcileFixture, c *copier.Copier) {
	t.Helper()
	require.NoError(t, c.Run(t.Context(), f.pool))
	require.True(t, c.Position().Watermark.Complete())
}

func newCopier(t *testing.T, f reconcileFixture, shadow schemachange.BuiltShadow) *copier.Copier {
	t.Helper()
	c, err := copier.NewCopier(f.target, shadow, f.lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	return c
}

// assertRepairsCoverExactly checks that the pass repaired at chunk
// granularity exactly where the gap wrote: every touched key lies in some
// repaired chunk, and no repaired chunk holds an untouched key. Keys, not a
// count, because a delete in the gap shifts the row-count chunker's
// boundaries after it.
func assertRepairsCoverExactly(t *testing.T, repairs []checksum.Repair, touched, untouched []int64) {
	t.Helper()
	require.NotEmpty(t, repairs)
	covered := func(key int64) bool {
		for _, r := range repairs {
			if r.Mismatch.Chunk.Lower() <= key && key <= r.Mismatch.Chunk.Upper() {
				return true
			}
		}
		return false
	}
	for _, key := range touched {
		assert.True(t, covered(key), "key %d was written during the gap and must lie in a repaired chunk", key)
	}
	for _, key := range untouched {
		assert.False(t, covered(key), "key %d was not written during the gap and its chunk must not have been recopied", key)
	}
}

// A run whose slot is dropped from under it reconciles rather than
// restarts: the walsender is killed mid catch-up, an operator drops the
// slot, the source moves while nothing captures it, and the resume reads
// the vanished slot as reconcile. Reconcile keeps the shadow, makes a new
// slot, repairs exactly the chunks the gap touched, and writes the row back
// to catching_up on the new slot; the capture reopened from the new slot's
// consistent point then carries the writes that follow, and the shadow
// converges and verifies clean.
func TestReconcileRecoversFromADroppedSlot(t *testing.T) {
	f := newReconcileFixture(t, 1500)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, decode.DropSlot(ctx, f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCopying, copier.Watermark{}, slot.Name(), slot.ConsistentPoint())))
	require.NoError(t, slot.Close(t.Context()))

	c := newCopier(t, f, shadow)
	first := f.startCapture(t, shadow, c, slot.ConsistentPoint())
	copyAll(t, f, c)
	f.exec(t, `UPDATE %s SET amount = amount + 1 WHERE id BETWEEN 1 AND 20`)
	status := first.awaitConfirmedPast(t, f.currentWALLSN(t))
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCatchingUp, c.Position().Watermark, slot.Name(), status.Confirmed)))

	// The failure: the walsender dies, and the slot goes with the writer.
	f.killWalsender(t, first, slot.Name())
	require.NoError(t, first.stream.Close(t.Context()))
	_, err = f.pool.Exec(t.Context(), `SELECT pg_catalog.pg_drop_replication_slot($1)`, slot.Name())
	require.NoError(t, err)

	// The gap: the source moves while nothing captures it — an update in
	// one chunk, a delete in another, inserts beyond the seeded keys.
	f.exec(t, `UPDATE %s SET amount = 0 WHERE id BETWEEN 301 AND 310`)
	f.exec(t, `DELETE FROM %s WHERE id BETWEEN 701 AND 705`)
	f.exec(t, `INSERT INTO %s (uniq, amount, label, blob) SELECT g, 1, 'gap-' || g, 'x' FROM generate_series(5001, 5010) AS g`)

	saved := f.load(t, shadow)
	resume, err := schemachange.InspectResume(t.Context(), f.pool, f.target, saved)
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeReconcile, resume.Mode)
	require.NotNil(t, resume.Lost)
	assert.Equal(t, &decode.SlotLostError{Slot: slot.Name()}, resume.Lost, "the slot is gone, not lost in place")
	assert.False(t, resume.Found)

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), f.pool, saved)
	require.NoError(t, err)
	require.NotNil(t, reconciled.Slot)
	t.Cleanup(func() { assert.NoError(t, reconciled.Slot.Close(context.WithoutCancel(t.Context()))) })
	assert.Equal(t, slot.Name(), reconciled.Slot.Name(), "the new slot wears the derived name")
	assert.Greater(t, reconciled.Slot.ConsistentPoint(), status.Confirmed, "the new slot starts after everything the old one delivered")

	assert.False(t, reconciled.Outcome.Clean(), "the gap left the shadow behind, so the pass repaired rather than proved")
	assertRepairsCoverExactly(t, reconciled.Outcome.Repairs, []int64{301, 310, 701, 705, 5001, 5010}, []int64{1, 20, 300, 500, 700})
	assert.Equal(t, checkpoint.PhaseCatchingUp, reconciled.Checkpoint.Phase, "a complete watermark resumes in catch-up")
	assert.Equal(t, slot.Name(), reconciled.Checkpoint.SlotName)
	assert.Equal(t, reconciled.Slot.ConsistentPoint(), reconciled.Checkpoint.LastAppliedLSN)
	stored := f.load(t, shadow)
	assert.Equal(t, reconciled.Checkpoint.Phase, stored.Phase)
	assert.Equal(t, reconciled.Checkpoint.LastAppliedLSN, stored.LastAppliedLSN)
	assert.True(t, stored.Watermark.Complete(), "the watermark survived the reconcile")
	f.assertConverged(t, shadow)

	// Capture resumes from the new slot and carries the writes after it.
	second := f.startCapture(t, shadow, c, reconciled.Slot.ConsistentPoint())
	f.exec(t, `UPDATE %s SET amount = amount * 2 WHERE id BETWEEN 1001 AND 1010`)
	f.exec(t, `DELETE FROM %s WHERE id BETWEEN 5001 AND 5003`)
	second.awaitConfirmedPast(t, f.currentWALLSN(t))
	second.stop(t)
	f.assertConverged(t, shadow)

	verifier, err := checksum.NewVerifier(f.target, shadow, f.lock, checksum.Options{})
	require.NoError(t, err)
	report, err := verifier.Verify(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean())
}

// A reconcile interrupted after its marker is finished by the next one:
// a row left in reconciling says reconcile whatever the catalog shows under
// the slot's name — here a slot intact and readable — and the restart drops
// that slot, makes a fresh one, and runs the pass again from the top.
func TestReconcileRestartsFromAnInterruptedReconcile(t *testing.T) {
	f := newReconcileFixture(t, 500)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	c := newCopier(t, f, shadow)
	copyAll(t, f, c)
	// The earlier attempt wrote its marker and made this slot, then died
	// before its pass; the source then moved.
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseReconciling, c.Position().Watermark, slot.Name(), slot.ConsistentPoint())))
	f.exec(t, `UPDATE %s SET amount = 0 WHERE id = 42`)

	saved := f.load(t, shadow)
	resume, err := schemachange.InspectResume(t.Context(), f.pool, f.target, saved)
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeReconcile, resume.Mode)
	assert.Nil(t, resume.Lost, "the row decided, not the slot")
	assert.True(t, resume.Found, "the catalog still shows the earlier attempt's slot")
	assert.False(t, resume.Slot.Lost())

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), f.pool, saved)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reconciled.Slot.Close(context.WithoutCancel(t.Context()))) })
	assert.Greater(t, reconciled.Slot.ConsistentPoint(), slot.ConsistentPoint(), "the earlier attempt's slot was replaced, not reused")
	assert.Len(t, reconciled.Outcome.Repairs, 1)
	assert.Equal(t, checkpoint.PhaseCatchingUp, f.load(t, shadow).Phase)
	f.assertConverged(t, shadow)
}

// A run that lost its slot before anything landed has nothing to repair:
// the reconcile makes the slot and writes the row back to copying, and the
// copier starts from the beginning under it.
func TestReconcileWithNothingLandedSkipsThePass(t *testing.T) {
	f := newReconcileFixture(t, 100)
	shadow := f.buildShadow(t)
	// The run saved its row before creating a slot and died there.
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCopying, copier.Watermark{}, "", 0)))

	saved := f.load(t, shadow)
	resume, err := schemachange.InspectResume(t.Context(), f.pool, f.target, saved)
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeReconcile, resume.Mode)
	assert.Equal(t, &decode.SlotLostError{Slot: f.target.DecodingName()}, resume.Lost, "a row with no slot never had one to read from")

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), f.pool, saved)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, reconciled.Slot.Close(ctx))
		assert.NoError(t, decode.DropSlot(ctx, f.cfg, f.pool, reconciled.Slot.Name()))
	})
	assert.Equal(t, checksum.Outcome{}, reconciled.Outcome, "no pass ran")
	assert.Equal(t, checkpoint.PhaseCopying, reconciled.Checkpoint.Phase)
	assert.Equal(t, f.target.DecodingName(), reconciled.Checkpoint.SlotName)
	assert.False(t, reconciled.Checkpoint.Watermark.Valid())
}

// A slot the catalog still has and the server still serves is a clean
// resume: nothing is dropped or repaired, and the row's slot status comes
// back for the caller to judge against its lag ceiling.
func TestInspectResumeReadsAnIntactSlotAsClean(t *testing.T) {
	f := newReconcileFixture(t, 100)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	cp := f.row(shadow, checkpoint.PhaseCopying, copier.NewWatermark(50), slot.Name(), slot.ConsistentPoint())

	resume, err := schemachange.InspectResume(t.Context(), f.pool, f.target, cp)
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeClean, resume.Mode)
	assert.Nil(t, resume.Lost)
	assert.True(t, resume.Found)
	assert.Equal(t, slot.Name(), resume.Slot.Name)
	assert.False(t, resume.Slot.Lost())
}

// A row a resume cannot act on is refused with a cause, before any slot
// is touched: a terminal phase, another table, another slot, another
// shadow, another statement.
func TestResumeRefusesARowItCannotActOn(t *testing.T) {
	f := newReconcileFixture(t, 100)
	shadow := f.buildShadow(t)
	name := f.target.DecodingName()
	good := f.row(shadow, checkpoint.PhaseCatchingUp, copier.NewWatermark(math.MaxInt64), name, 0)

	done := good
	done.Phase = checkpoint.PhaseDone
	_, err := schemachange.InspectResume(t.Context(), f.pool, f.target, done)
	assert.Equal(t, schemachange.CauseResumeTerminal, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)

	otherTable := good
	otherTable.Table = "elsewhere"
	_, err = schemachange.InspectResume(t.Context(), f.pool, f.target, otherTable)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))

	otherSlot := good
	otherSlot.SlotName = "pgsprite_00000000"
	_, err = schemachange.InspectResume(t.Context(), f.pool, f.target, otherSlot)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{})
	require.NoError(t, err)
	otherShadow := good
	otherShadow.ShadowTable = "not_the_shadow"
	_, err = reconciler.Reconcile(t.Context(), f.pool, otherShadow)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))

	otherStatement := good
	otherStatement.TargetFingerprint = "other"
	_, err = reconciler.Reconcile(t.Context(), f.pool, otherStatement)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))

	_, found, err := decode.InspectSlot(t.Context(), f.pool, name)
	require.NoError(t, err)
	assert.False(t, found, "no refusal made a slot")
}

// A target proven for a run that does not decode WAL has no slot to lose:
// its resume is always clean, and there is no reconcile mode for it.
func TestQuiescedTargetHasNoReconcileMode(t *testing.T) {
	f := newReconcileFixture(t, 100)
	quiesced, err := preflight.CheckCopySwap(t.Context(), f.pool, f.table.Schema, f.table.Table,
		preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	shadow := f.buildShadow(t)
	cp := f.row(shadow, checkpoint.PhaseCopying, copier.NewWatermark(50), "", 0)

	resume, err := schemachange.InspectResume(t.Context(), f.pool, quiesced, cp)
	require.NoError(t, err)
	assert.Equal(t, schemachange.Resume{Mode: schemachange.ResumeClean}, resume)

	_, err = schemachange.NewReconciler(f.cfg, f.lock, f.store, quiesced, shadow, checksum.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.False(t, errors.As(err, new(*schemachange.RefusalError)), "a programmer error, not a catalog refusal")
}
