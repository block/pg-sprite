package checksum_test

import (
	"context"
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
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// liveFixture is one database on a cluster that decodes WAL, holding the
// seeded workload table and its copy-and-swap proof, with the engine's
// catch-up stage wired in front of the verifier: slot, shadow, stream,
// flusher, a copy run to completion, and the catch-up applying the stream
// on its own goroutine. Every live pass below runs against that catch-up.
type liveFixture struct {
	cfg     dbconn.Config
	pool    *pgxpool.Pool
	table   testutil.WorkloadTable
	target  preflight.CopySwapTarget
	lock    *dbconn.TableLockSession
	shadow  schemachange.BuiltShadow
	catchup *applier.Catchup
	cancel  context.CancelFunc
	done    chan error
}

// liveRows is the size of the workload table every live pass compares:
// keys 1..liveRows, so chunkRows cuts [MinInt64, 1000], [1001, 2000], and
// [2001, MaxInt64].
const liveRows = 2500

// heldFlushLockTimeout is the flusher's lock wait in a fixture whose
// shadow a test holds a lock on: long enough for the test to release the
// hold before the flush gives up and ends the catch-up.
const heldFlushLockTimeout = 30 * time.Second

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s")
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, serverURL), MaxConns: 16}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	table := testutil.NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), liveRows))
	target, err := preflight.CheckCopySwap(t.Context(), pool, table.Schema, table.Table,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	f := &liveFixture{cfg: cfg, pool: pool, table: table, target: target, done: make(chan error, 1)}
	f.start(t)
	return f
}

// start wires the catch-up the way the orchestrator will: the slot created
// under the table lock before the shadow is built, the stream opened from
// its consistent point, the copy run to completion so every key is landed,
// and the catch-up started against the finished copier's position.
func (f *liveFixture) start(t *testing.T) {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.table.Schema, f.table.Table)
	require.NoError(t, err)
	t.Cleanup(func() {
		if lock.Err() == nil {
			assert.NoError(t, lock.Release(context.WithoutCancel(t.Context())))
		}
	})
	f.lock = lock

	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, slot.Close(ctx))
		assert.NoError(t, decode.DropSlot(ctx, f.cfg, f.pool, slot.Name()))
	})

	alter, err := statement.ParseOne(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN label`, f.table.Qualified()))
	require.NoError(t, err)
	f.shadow, err = schemachange.BuildShadow(t.Context(), f.pool, lock, f.target, alter, schemachange.Options{})
	require.NoError(t, err)

	stream, err := decode.OpenStream(t.Context(), f.cfg, f.pool, f.target, slot.ConsistentPoint())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, stream.Close(context.WithoutCancel(t.Context())))
	})

	flusher, err := applier.NewFlusher(f.target, f.shadow, lock, applier.Options{LockTimeout: heldFlushLockTimeout})
	require.NoError(t, err)
	c, err := copier.NewCopier(f.target, f.shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 50, MaxRows: 50},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))
	require.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Cut, "every key is landed once the copy returns")

	f.catchup, err = applier.NewCatchup(stream, flusher, c, applier.CatchupOptions{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel = cancel
	go func() { f.done <- f.catchup.Run(ctx, f.pool) }()
	t.Cleanup(func() {
		cancel()
		const stopDeadline = 15 * time.Second
		select {
		case err := <-f.done:
			assert.ErrorIs(t, err, context.Canceled, "the catch-up ran until the test ended")
		case <-time.After(stopDeadline):
			t.Errorf("catch-up did not return within %s of its context ending", stopDeadline)
		}
	})
}

// currentWALLSN reads the server's current write position; a marker a
// pass has already written lies at or below it.
func (f *liveFixture) currentWALLSN(t *testing.T) decode.LSN {
	t.Helper()
	var text string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_current_wal_lsn()::text`).Scan(&text))
	lsn, err := decode.ParseLSN(text)
	require.NoError(t, err)
	return lsn
}

// shadowName is the schema-qualified, quoted shadow table.
func (f *liveFixture) shadowName() string {
	return pgx.Identifier{f.table.Schema, f.shadow.ShadowTable()}.Sanitize()
}

// exec runs one statement on the pool.
func (f *liveFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), sql)
	require.NoError(t, err)
}

// holdShadow takes a SHARE MODE lock on the shadow in a transaction of its
// own, so the stream's flushes wait and the shadow holds still while the
// hold lasts; the catch-up's confirmed position stays below every change
// those flushes carry. The returned release commits the transaction and
// is safe to call more than once. The flusher's lock wait outlasts the
// hold, so a test that releases it keeps its catch-up.
func (f *liveFixture) holdShadow(t *testing.T) (release func()) {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "LOCK TABLE "+f.shadowName()+" IN SHARE MODE")
	require.NoError(t, err)
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		require.NoError(t, tx.Commit(context.WithoutCancel(t.Context())))
	}
	t.Cleanup(release)
	return release
}

// verifier builds a verifier over fixed thousand-row chunks.
func (f *liveFixture) verifier(t *testing.T, opts checksum.Options) *checksum.Verifier {
	t.Helper()
	opts.Chunker = chunkRows
	v, err := checksum.NewVerifier(f.target, f.shadow, f.lock, opts)
	require.NoError(t, err)
	return v
}

// checkLive runs one live pass through the complete watermark.
func (f *liveFixture) checkLive(t *testing.T, v *checksum.Verifier, policy checksum.DivergencePolicy, live checksum.LiveOptions) (checksum.Outcome, error) {
	t.Helper()
	if live.Confirmed == nil {
		live.Confirmed = f.catchup
	}
	return v.CheckLive(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64), policy, live)
}

// assertConverged compares the source with its shadow on every column the
// schema change kept.
func (f *liveFixture) assertConverged(t *testing.T) {
	t.Helper()
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.table.Schema, Table: f.shadow.SourceTable()},
		testutil.RelationRef{Schema: f.table.Schema, Table: f.shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"label"}})
}

// pollSleep is the wall-clock sleep a test hands a live pass once it has
// done what it wanted to at the first wait.
func pollSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// confirmedFunc adapts a function to checksum.ConfirmedSource, so a test
// can stand in for the catch-up's position.
type confirmedFunc func() decode.LSN

func (fn confirmedFunc) Confirmed() decode.LSN { return fn() }
