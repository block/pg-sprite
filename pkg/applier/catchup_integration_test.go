package applier_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// The convergence tests below run the engine's catch-up stage end to end
// against a real decoded stream: a workload mutates the source while the
// copier fills the shadow and the catch-up applies what the stream decodes,
// then the workload stops, the catch-up confirms past the last commit, and
// the source and the shadow are compared row by row. Each test aims one
// race at the stage; the comparison is the oracle for all of them.

// catchupFixture is one database on a cluster that decodes WAL, holding
// the seeded workload table and the copy-and-swap proof minted for a run
// that decodes WAL. The pool is sized for the workload's workers, the
// copier's chunks, and the flush to share it without starving each other.
type catchupFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	table  testutil.WorkloadTable
	target preflight.CopySwapTarget
}

// walSenderTimeout keeps the server's keepalives frequent, so a quiet
// stream's delivered position reaches the server's write position within
// a second of the workload stopping.
const walSenderTimeout = "wal_sender_timeout=2s"

func newCatchupFixture(t *testing.T, rows int) catchupFixture {
	t.Helper()
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", walSenderTimeout)
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, serverURL), MaxConns: 16}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	table := testutil.NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), rows))
	target, err := preflight.CheckCopySwap(t.Context(), pool, table.Schema, table.Table,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	return catchupFixture{cfg: cfg, pool: pool, table: table, target: target}
}

// currentWALLSN is the server's write position: once the workload has
// stopped, every one of its commits lies at or below it.
func (f catchupFixture) currentWALLSN(t *testing.T) decode.LSN {
	t.Helper()
	var text string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_current_wal_lsn()::text`).Scan(&text))
	lsn, err := decode.ParseLSN(text)
	require.NoError(t, err)
	return lsn
}

// assertConverged compares the source with its shadow on every column the
// schema change kept.
func (f catchupFixture) assertConverged(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.table.Schema, Table: shadow.SourceTable()},
		testutil.RelationRef{Schema: f.table.Schema, Table: shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"label"}})
}

// catchupRun is the catch-up stage wired the way the orchestrator will wire
// it: the slot created under the table lock before the shadow is built, so
// the slot's consistent point precedes every change the copier could miss;
// the stream opened from that point; the copier and the catch-up sharing
// the lock session and the copier's position. Run is started on its own
// goroutine before the copier, so the catch-up judges keys against a copier
// that has claimed nothing — every key uncut — until copy starts it.
type catchupRun struct {
	f       catchupFixture
	shadow  schemachange.BuiltShadow
	lock    *dbconn.TableLockSession
	copier  *copier.Copier
	catchup *applier.Catchup
	cancel  context.CancelFunc
	done    chan error
}

// schemaChange is the change every test here applies: the workload table
// loses its label column, so the shadow's column list is narrower than the
// source's and the flush writes the columns the shadow kept.
const schemaChange = `ALTER TABLE %s DROP COLUMN label`

// catchupSetup is how a test wires its catch-up. The zero value is the
// orchestrator's wiring under schemaChange: the catch-up judges keys
// against the live copier, which the test runs with copy.
type catchupSetup struct {
	// change is the schema change the shadow is built for; empty means
	// schemaChange.
	change string
	opts   applier.CatchupOptions
	// lockOpts shape the table lock session the copier and the flusher share.
	lockOpts []dbconn.TableLockOption
	// positions, when set, replaces the copier as the catch-up's judge of
	// keys. The copy then runs to completion before the catch-up starts, so
	// the shadow holds every row, and the test moves the position by hand.
	positions applier.PositionSource
}

func startCatchup(t *testing.T, f catchupFixture, opts applier.CatchupOptions) *catchupRun {
	t.Helper()
	return startCatchupWith(t, f, catchupSetup{opts: opts})
}

func startCatchupWith(t *testing.T, f catchupFixture, setup catchupSetup) *catchupRun {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.table.Schema, f.table.Table, setup.lockOpts...)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := lock.Release(context.WithoutCancel(t.Context()))
		if lock.Err() != nil {
			assert.Error(t, err, "releasing a session that lost its lock reports the loss")
			return
		}
		assert.NoError(t, err)
	})

	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, slot.Close(ctx))
		assert.NoError(t, decode.DropSlot(ctx, f.cfg, f.pool, slot.Name()))
	})

	change := setup.change
	if change == "" {
		change = schemaChange
	}
	alter, err := statement.ParseOne(fmt.Sprintf(change, pgx.Identifier{f.table.Schema, f.table.Table}.Sanitize()))
	require.NoError(t, err)
	shadow, err := schemachange.BuildShadow(t.Context(), f.pool, lock, f.target, alter, schemachange.Options{})
	require.NoError(t, err)

	stream, err := decode.OpenStream(t.Context(), f.cfg, f.pool, f.target, slot.ConsistentPoint())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, stream.Close(context.WithoutCancel(t.Context())))
	})

	flusher, err := applier.NewFlusher(f.target, shadow, lock, applier.Options{})
	require.NoError(t, err)
	// Small chunks and two workers keep in-flight chunks in the copier's
	// position for most of the copy, so a change's key is as likely to be
	// in flight as landed when the drain judges it.
	c, err := copier.NewCopier(f.target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 50, MaxRows: 50},
	})
	require.NoError(t, err)
	run := &catchupRun{f: f, shadow: shadow, lock: lock, copier: c, done: make(chan error, 1)}
	positions := applier.PositionSource(c)
	if setup.positions != nil {
		run.copy(t)
		positions = setup.positions
	}
	catchup, err := applier.NewCatchup(stream, flusher, positions, setup.opts)
	require.NoError(t, err)
	run.catchup = catchup

	ctx, cancel := context.WithCancel(t.Context())
	run.cancel = cancel
	go func() { run.done <- catchup.Run(ctx, f.pool) }()
	t.Cleanup(func() {
		// A test that stopped the run already has its result; this is the
		// stop path for one that failed before it could, and it only has
		// to see the goroutine return.
		cancel()
		_ = run.wait(t)
	})
	return run
}

// copy runs the copier to completion while the catch-up keeps consuming.
func (r *catchupRun) copy(t *testing.T) {
	t.Helper()
	require.NoError(t, r.copier.Run(t.Context(), r.f.pool))
	require.Equal(t, copier.NewWatermark(math.MaxInt64), r.copier.Position().Cut, "every key is landed once the copy returns")
}

// status is the catch-up's status, failing the test if Run has returned,
// since a status after that would never move again.
func (r *catchupRun) status(t *testing.T) applier.Status {
	t.Helper()
	select {
	case err := <-r.done:
		t.Fatalf("catch-up returned while the test still needs it running: %v", err)
	default:
	}
	return r.catchup.Status()
}

// awaitStatus polls the catch-up's status until want holds, failing with
// the last status seen when the deadline passes first.
func (r *catchupRun) awaitStatus(t *testing.T, what string, want func(applier.Status) bool) {
	t.Helper()
	const statusDeadline = 30 * time.Second
	deadline := time.NewTimer(statusDeadline)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		s := r.status(t)
		if want(s) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("catch-up did not %s within %s; last status %+v", what, statusDeadline, s)
		case <-poll.C:
		}
	}
}

// quiesce stops the workload and waits for the catch-up to confirm past
// the server's write position, at which point every change the workload
// committed is in the shadow or was discarded as a change the copy itself
// read. A buffered entry holds the confirmation below its position, so a
// confirmation past the end also means the buffer holds nothing from the
// workload. The catch-up's own measurement of the server's write position
// reaches the same point, since it is read on the pool after the
// confirmation, not taken from the walsender.
func (r *catchupRun) quiesce(t *testing.T, load *testutil.LoadGenerator) testutil.Summary {
	t.Helper()
	summary, err := load.Stop()
	require.NoError(t, err)
	end := r.f.currentWALLSN(t)
	r.awaitStatus(t, fmt.Sprintf("confirm past the workload's last commit at %s and measure the server's write position", end), func(s applier.Status) bool {
		return s.Confirmed >= end && s.WALEnd >= end
	})
	return summary
}

// stop ends the run and checks it reports the stop as the caller's, not as
// the stream's receive error.
func (r *catchupRun) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	err := r.wait(t)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "catch-up on "+r.f.table.Schema+"."+r.f.table.Table+" stopped")
}

func (r *catchupRun) wait(t *testing.T) error {
	t.Helper()
	const stopDeadline = 15 * time.Second
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(stopDeadline):
		t.Fatalf("catch-up did not return within %s of its context ending", stopDeadline)
		return nil
	}
}

// catchUpTracker is a tracker mid catch-up step, the way the orchestrator
// will hand one to the catch-up: the step is the caller's, the counters
// the catch-up's.
func catchUpTracker(t *testing.T) *progress.Tracker {
	t.Helper()
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationCatchUp)
	tracker.StartStep(1, progress.OperationCatchUp, "")
	return tracker
}

// Inserts, updates, and deletes arrive while the copier has claimed
// nothing, while it is mid copy, and after it has landed every key. The
// first are discarded, since the copy's own read sees them; the last are
// applied; the shadow converges all the same (CO-4). The tracker a caller
// hands the catch-up carries its counters while it runs and not after.
func TestCatchupConvergesUnderMixedLoad(t *testing.T) {
	f := newCatchupFixture(t, 1500)
	tracker := catchUpTracker(t)
	run := startCatchup(t, f, applier.CatchupOptions{Tracker: tracker})
	load := testutil.StartLoad(t, f.pool, f.table, testutil.LoadSpec{
		Seed: 1, Workers: 4, RatePerSecond: 60,
		Mix:                  testutil.Mix{Insert: 1, Update: 2, Delete: 1},
		ToastRewriteFraction: 0.5,
	})

	run.awaitStatus(t, "discard changes to keys the copier has not claimed", func(s applier.Status) bool {
		return s.Discarded > 0
	})
	run.copy(t)
	run.awaitStatus(t, "apply changes to landed keys", func(s applier.Status) bool {
		return s.Applied > 0
	})
	mid, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	require.NotNil(t, mid.Detail.Work, "a poll during the catch-up carries its work")
	assert.Positive(t, mid.Detail.Work.ChangesApplied)

	summary := run.quiesce(t, load)
	assert.Positive(t, summary.Inserts)
	assert.Positive(t, summary.Updates)
	assert.Positive(t, summary.Deletes)
	final := run.status(t)
	assert.Zero(t, final.Buffered, "nothing waits once the copy has landed and the stream has passed every completion")
	assert.Zero(t, final.BatchesDeferred, "no unique key moves, so no batch is refused")

	run.stop(t)
	f.assertConverged(t, run.shadow)
	after, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a returned catch-up is no longer the tracker's work source")
	direct, err := run.catchup.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, final.Applied, direct.ChangesApplied, "the applied count survives Run")
}

// A row's primary key moves to a fresh identity value, so the change is an
// image under a new key paired with a delete under the old one. The pair
// travels together through the drain, and the flush completes the image's
// unchanged blob from the old key's row before it deletes that row (CO-4,
// D13). The workload starts after the copy has landed every key: the moved
// row keeps its unique key, so mid copy the copier's own insert of the row
// under its new key collides with the stale row the flush has not deleted
// yet, which is the copier's race to resolve, not the catch-up's.
func TestCatchupConvergesUnderKeyMoves(t *testing.T) {
	f := newCatchupFixture(t, 1500)
	run := startCatchup(t, f, applier.CatchupOptions{})
	run.copy(t)
	load := testutil.StartLoad(t, f.pool, f.table, testutil.LoadSpec{
		Seed: 2, Workers: 4, RatePerSecond: 60,
		Mix: testutil.Mix{KeyMove: 2, Update: 1},
	})

	run.awaitStatus(t, "apply moved images and the deletes under their old keys", func(s applier.Status) bool {
		return s.Applied > 0
	})
	summary := run.quiesce(t, load)
	assert.Positive(t, summary.KeyMoves)

	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// Every update lands on one of the ten lowest keys, so the same rows change
// many times within one cycle and across cycles while their chunk is
// uncut, in flight, and landed. The buffer keeps one entry per key — the
// latest image — and the shadow converges on it (CO-5).
func TestCatchupConvergesUnderHotRowContention(t *testing.T) {
	f := newCatchupFixture(t, 1500)
	run := startCatchup(t, f, applier.CatchupOptions{})
	load := testutil.StartLoad(t, f.pool, f.table, testutil.LoadSpec{
		Seed: 3, Workers: 4, RatePerSecond: 80,
		Mix:            testutil.Mix{Update: 1},
		HotRowFraction: 1,
	})

	run.awaitStatus(t, "discard changes to keys the copier has not claimed", func(s applier.Status) bool {
		return s.Discarded > 0
	})
	run.copy(t)
	run.awaitStatus(t, "apply changes to landed keys", func(s applier.Status) bool {
		return s.Applied > 0
	})
	summary := run.quiesce(t, load)
	assert.Positive(t, summary.Updates)

	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// No update rewrites the out-of-line blob column, so every decoded image
// omits it: the unchanged-TOAST marker. A landed key's image is written
// column-wise and leaves the shadow's blob in place; the comparison covers
// the blob, so a marker written as a value would show (CO-8).
func TestCatchupConvergesUnderUnchangedTOASTUpdates(t *testing.T) {
	f := newCatchupFixture(t, 1500)
	run := startCatchup(t, f, applier.CatchupOptions{})
	load := testutil.StartLoad(t, f.pool, f.table, testutil.LoadSpec{
		Seed: 4, Workers: 4, RatePerSecond: 60,
		Mix:                  testutil.Mix{Update: 1},
		ToastRewriteFraction: 0,
	})

	run.awaitStatus(t, "discard changes to keys the copier has not claimed", func(s applier.Status) bool {
		return s.Discarded > 0
	})
	run.copy(t)
	run.awaitStatus(t, "apply changes to landed keys", func(s applier.Status) bool {
		return s.Applied > 0
	})
	summary := run.quiesce(t, load)
	assert.Positive(t, summary.Updates)

	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// Two rows swap their unique keys inside one transaction through a parked
// value. The buffer keeps each row's final image, so the column-wise flush
// writes one row's new unique key while the other row still holds it in
// the shadow, the unique index refuses, and the flush applies the batch
// whole-row instead (CO-6). The workload starts after the copy has landed
// every key: a swap mid copy can straddle a chunk the copier is reading.
func TestCatchupConvergesUnderUniqueKeyMoves(t *testing.T) {
	f := newCatchupFixture(t, 1500)
	run := startCatchup(t, f, applier.CatchupOptions{})
	run.copy(t)
	load := testutil.StartLoad(t, f.pool, f.table, testutil.LoadSpec{
		Seed: 5, Workers: 2, RatePerSecond: 40,
		Mix: testutil.Mix{UniqueMove: 1},
	})

	run.awaitStatus(t, "fall back to whole-row writes for a unique key swap", func(s applier.Status) bool {
		return s.Fallbacks > 0
	})
	summary := run.quiesce(t, load)
	assert.Positive(t, summary.UniqueMoves)

	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// A second Run on the same catch-up is refused: its stream is single-use.
func TestCatchupRunsOnce(t *testing.T) {
	f := newCatchupFixture(t, 10)
	run := startCatchup(t, f, applier.CatchupOptions{})
	run.copy(t)
	run.stop(t)
	err := run.catchup.Run(t.Context(), f.pool)
	require.ErrorIs(t, err, applier.ErrCatchupAlreadyRun)
	assert.False(t, errors.Is(err, context.Canceled))
}
