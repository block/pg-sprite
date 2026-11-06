package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here prove what the copy-and-swap route's pool floor buys. Each
// copy worker holds a pool connection for as long as its chunk transaction
// lasts, and a chunk whose insert meets an uncommitted shadow row under one
// of its keys lasts until that row's transaction ends. With every worker
// held that way, a pool of CopySwapMinConns still has a connection for the
// catch-up's flush and one for the checkpoint saves, so a write to the
// source lands in the shadow while the copy stands still; a pool of only
// the workers has neither, and the same write waits in the stream until a
// worker frees its connection. The tests stand the workers still by holding
// shadow rows themselves: a transaction inserts, and does not commit, a
// shadow row for one key of each held chunk, and the chunk's insert waits
// on that transaction's id.

// poolFloorWorkers is how many copy workers the runs here use, so the floor
// they are held to is this many plus the two the route reserves.
const poolFloorWorkers = 4

// poolFloorChunkRows is the fixed chunk size: with liveSeededRows rows
// seeded with keys 1..liveSeededRows, chunk k covers keys
// (k-1)*poolFloorChunkRows+1 .. k*poolFloorChunkRows, and the seeded rows
// fill exactly poolFloorChunks chunks.
const poolFloorChunkRows = 500

// poolFloorChunks is how many chunks the seeded rows fill. The held chunks
// are the second half: the first half lands freely, which keeps the holds
// clear of the copy's start, and every worker then stands on a held chunk.
const poolFloorChunks = liveSeededRows / poolFloorChunkRows

const (
	// poolFloorPoll paces every poll here.
	poolFloorPoll = 10 * time.Millisecond
	// poolFloorDeadline bounds each wait for the run to reach a state: the
	// workers blocking, a save landing, a flushed row appearing.
	poolFloorDeadline = 30 * time.Second
	// poolStarvedWindow is how long the test watches a write that has no
	// connection to be flushed on stay out of the shadow.
	poolStarvedWindow = 1500 * time.Millisecond
	// poolHoldTimeout bounds the lock waits inside a chunk transaction while
	// a test holds its keys, in place of the copier's default: the wait is
	// the test's own doing and ends when the test releases the hold, so the
	// bound only has to outlast the test's deadlines.
	poolHoldTimeout = time.Minute
)

// flushedLabel is the value the source write under test carries.
const flushedLabel = "flushed while every worker waited"

// poolFloorCopier is the copy the runs here are sized by: fixed chunks, so
// each held key falls in a known chunk, and lock waits bounded by
// poolHoldTimeout, so the held chunks wait for the test rather than fail.
func poolFloorCopier() copier.Options {
	return copier.Options{
		Workers:          poolFloorWorkers,
		LockTimeout:      poolHoldTimeout,
		StatementTimeout: poolHoldTimeout,
		Chunker: copier.ChunkerOptions{
			InitialRows: poolFloorChunkRows,
			MinRows:     poolFloorChunkRows,
			MaxRows:     poolFloorChunkRows,
		},
	}
}

// chunkKey is the first key of chunk k under poolFloorCopier's chunking: a
// row held under it blocks chunk k and no other.
func chunkKey(k int) int64 {
	return int64((k-1)*poolFloorChunkRows + 1)
}

// heldKeys is one key per chunk from first through last inclusive.
func heldKeys(first, last int) []int64 {
	keys := make([]int64, 0, last-first+1)
	for k := first; k <= last; k++ {
		keys = append(keys, chunkKey(k))
	}
	return keys
}

// firstHeldChunk is the first chunk the tests hold: the one after the
// freely landing first half.
const firstHeldChunk = poolFloorChunks/2 + 1

// shadowName is the run's shadow table, qualified.
func (f liveFixture) shadowName() string {
	return pgx.Identifier{f.table.Schema, schemachange.ShadowName(f.table.Schema, f.table.Table)}.Sanitize()
}

// runPool opens a pool of exactly size connections on the fixture's
// database, the pool the run under test is handed.
func (f liveFixture) runPool(t *testing.T, size int32) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: f.url, MaxConns: size})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.Equal(t, size, pool.Config().MaxConns)
	return pool
}

// waitForCopyWriting waits until a transaction of the run holds the shadow
// for writing: the copier's resume fence, which takes the shadow in a mode
// that admits no writer, has committed, and the first chunks are being
// inserted. A hold opened before the fence would stand in the fence's way
// instead of a chunk's; one opened now is admitted at once, and the chunks
// it holds are not cut until a chunk of the first half has landed.
func (f liveFixture) waitForCopyWriting(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		var writing bool
		err := f.pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_locks
				WHERE locktype = 'relation'
				  AND granted
				  AND mode = 'RowExclusiveLock'
				  AND relation = to_regclass($1)::oid)`, f.shadowName()).Scan(&writing)
		require.NoError(t, err)
		return writing
	}, poolFloorDeadline, poolFloorPoll, "a transaction of the run holds the shadow for writing")
}

// holdShadowRows opens a transaction on the fixture's own pool that
// inserts the source rows under keys into the shadow and leaves them
// uncommitted, and returns it for the test to roll back. A chunk whose
// insert reaches one of these keys waits on this transaction, and the
// rollback leaves nothing behind: the chunk then inserts the source row
// with no conflict. The columns are the workload table's; the key is
// written explicitly, which its identity, generated by default, allows.
func (f liveFixture) holdShadowRows(t *testing.T, keys []int64) pgx.Tx {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { releaseLeftover(t, tx) })
	_, err = tx.Exec(t.Context(), `
		INSERT INTO `+f.shadowName()+` (id, uniq, amount, label, blob, updated_at)
		SELECT id, uniq, amount, label, blob, updated_at
		FROM `+f.table.Qualified()+`
		WHERE id = ANY($1::bigint[])`, keys)
	require.NoError(t, err, "hold shadow rows %v", keys)
	return tx
}

// release rolls a hold back so the chunks it held proceed.
func release(t *testing.T, hold pgx.Tx) {
	t.Helper()
	require.NoError(t, hold.Rollback(t.Context()))
}

// releaseLeftover rolls back, at cleanup, a hold the test did not release
// itself — a failed assertion before the release would otherwise leave the
// run, and so the test, waiting on it. A hold the test did release reports
// the closed transaction, which is the expected answer here.
func releaseLeftover(t *testing.T, hold pgx.Tx) {
	t.Helper()
	err := hold.Rollback(context.WithoutCancel(t.Context()))
	if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("roll back a leftover hold: %v", err)
	}
}

// blockedWorkers counts the copy workers waiting on another transaction's
// id inside a chunk insert: the chunk's first pass, which inserts into the
// shadow what it selects from the source. The catch-up's flush inserts
// into the shadow too, from values rather than from the source, and waits
// on no held row here, so the selection from the source tells the two
// apart.
func (f liveFixture) blockedWorkers(ctx context.Context) (int, error) {
	var n int
	err := f.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database()
		  AND wait_event_type = 'Lock'
		  AND wait_event = 'transactionid'
		  AND starts_with(query, $1)
		  AND position($2 IN query) > 0`,
		"INSERT INTO "+f.shadowName(), " FROM "+f.table.Qualified()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count blocked copy workers: %w", err)
	}
	return n, nil
}

// waitForBlockedWorkers waits until exactly want copy workers are blocked.
func (f liveFixture) waitForBlockedWorkers(t *testing.T, want int) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		n, err := f.blockedWorkers(t.Context())
		require.NoError(t, err)
		return n == want
	}, poolFloorDeadline, poolFloorPoll, "%d copy workers blocked on a held shadow row", want)
}

// assertBlockedWorkers asserts that exactly want copy workers are blocked
// right now.
func (f liveFixture) assertBlockedWorkers(t *testing.T, want int) {
	t.Helper()
	n, err := f.blockedWorkers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, n, "copy workers blocked on a held shadow row")
}

// shadowHasLabel reports whether the shadow row under id carries label.
func (f liveFixture) shadowHasLabel(t *testing.T, id int64, label string) bool {
	t.Helper()
	var got string
	err := f.pool.QueryRow(t.Context(), `
		SELECT label
		FROM `+f.shadowName()+`
		WHERE id = $1`, id).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	require.NoError(t, err)
	return got == label
}

// writeSource updates the label of the source row under id, the write
// whose arrival in the shadow the tests watch.
func (f liveFixture) writeSource(t *testing.T, id int64, label string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), `
		UPDATE `+f.table.Qualified()+`
		SET label = $2
		WHERE id = $1`, id, label)
	require.NoError(t, err)
}

// landedWatch is the checkpoint hook of the runs here: it records the
// highest landed watermark any save reported, so a test can wait for a
// chunk to have landed.
type landedWatch struct {
	mu     sync.Mutex
	landed int64
}

func (w *landedWatch) onCheckpoint(_ context.Context, cp checkpoint.Checkpoint) error {
	if !cp.Watermark.Valid() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.landed = max(w.landed, cp.Watermark.Value())
	return nil
}

// waitForLanded waits until a save has reported the copy landed through
// key at least.
func (w *landedWatch) waitForLanded(t *testing.T, key int64) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.landed >= key
	}, poolFloorDeadline, poolFloorPoll, "a checkpoint save reporting the copy landed through key %d", key)
}

// At the floor, the catch-up flushes while every copy worker waits. The
// run's pool holds CopySwapMinConns connections for four workers; the test
// holds one key of each chunk of the second half, so the first half lands
// and the four workers then stand on the four held chunks, each holding a
// connection. A source write to key 1, a landed key, is flushed into the
// shadow on one of the two connections the floor keeps, and the checkpoint
// save that reported the first half landed went through on the other.
// Releasing the first held chunk alone lands it and no more — the freed
// worker finds only the empty chunk above the last key and is done, and
// three workers still wait — which is the control that the holds are what
// stands the workers still. Releasing the rest lets the run finish, and
// both oracles hold on the result.
func TestRunCopySwapFlushesAtThePoolFloorWhileEveryWorkerWaits(t *testing.T) {
	shape := testutil.WorkloadShape{IDType: "integer"}
	f := newLiveFixture(t, shape)
	const alter = `ALTER TABLE %s ALTER COLUMN id TYPE bigint`
	copyOpts := poolFloorCopier()
	pool := f.runPool(t, migrate.CopySwapMinConns(copyOpts))

	watch := &landedWatch{}
	opts := f.options()
	opts.CopySwap.Copier = copyOpts
	opts.CopySwap.CheckpointInterval = 50 * time.Millisecond
	opts.CopySwap.OnCheckpoint = watch.onCheckpoint

	var (
		wg sync.WaitGroup
		v  verdict.Verdict
		// runErr is set once wg is done.
		runErr error
	)
	wg.Go(func() {
		v, runErr = migrate.Run(context.WithoutCancel(t.Context()), pool, parseOne(t, fmt.Sprintf(alter, f.table.Qualified())), opts)
	})

	f.waitForCopyWriting(t)
	holdFirst := f.holdShadowRows(t, heldKeys(firstHeldChunk, firstHeldChunk))
	holdRest := f.holdShadowRows(t, heldKeys(firstHeldChunk+1, poolFloorChunks))

	f.waitForBlockedWorkers(t, poolFloorWorkers)
	watch.waitForLanded(t, chunkKey(firstHeldChunk)-1)

	f.writeSource(t, 1, flushedLabel)
	require.Eventually(t, func() bool { return f.shadowHasLabel(t, 1, flushedLabel) },
		poolFloorDeadline, poolFloorPoll, "the write to a landed key is flushed into the shadow while every worker waits")
	f.assertBlockedWorkers(t, poolFloorWorkers)

	release(t, holdFirst)
	watch.waitForLanded(t, chunkKey(firstHeldChunk+1)-1)
	f.waitForBlockedWorkers(t, poolFloorWorkers-1)

	release(t, holdRest)
	wg.Wait()
	require.NoError(t, runErr)
	require.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	f.assertZeroDiff(t, shape, alter)
}

// Below the floor, the same write waits behind the copy. Run refuses a
// pool of only the workers before anything is read, so the test drives
// the Runner directly with one, the way a host that sized its own pool
// wrongly would reach it. With the four workers standing on the held
// chunks, the source write to key 1 stays out of the shadow for the whole
// watched window: the catch-up's flush and its progress reads are queued
// on the pool behind the workers. The catch-up reads no keepalive while
// it waits there, so the server's sender timeout is set to outlast the
// window; under the live fixture's short one the server would drop the
// stream instead, which is the other face of the same starvation.
// Releasing the hold frees the workers, the write lands, and the run
// finishes.
func TestRunnerCopySwapFlushWaitsBehindTheCopyWhenThePoolHoldsOnlyTheWorkers(t *testing.T) {
	shape := testutil.WorkloadShape{IDType: "integer"}
	f := newLiveFixtureOn(t, shape, "wal_sender_timeout=60s")
	const alter = `ALTER TABLE %s ALTER COLUMN id TYPE bigint`
	copyOpts := poolFloorCopier()
	pool := f.runPool(t, poolFloorWorkers)

	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.table.Schema, f.table.Table,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	runner, err := schemachange.NewRunner(dbconn.Config{URL: f.url}, store, schemachange.RunOptions{
		Copier:             copyOpts,
		KeepOld:            true,
		CheckpointInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	var (
		wg     sync.WaitGroup
		result schemachange.Result
		// runErr is set once wg is done.
		runErr error
	)
	wg.Go(func() {
		result, runErr = runner.Run(context.WithoutCancel(t.Context()), pool, target, parseOne(t, fmt.Sprintf(alter, f.table.Qualified())))
	})

	f.waitForCopyWriting(t)
	hold := f.holdShadowRows(t, heldKeys(firstHeldChunk, poolFloorChunks))

	// Four blocked workers means the first half landed: each worker stands
	// on a held chunk only after landing a chunk of the first half.
	f.waitForBlockedWorkers(t, poolFloorWorkers)

	f.writeSource(t, 1, flushedLabel)
	assert.Never(t, func() bool { return f.shadowHasLabel(t, 1, flushedLabel) },
		poolStarvedWindow, poolFloorPoll, "the write to a landed key reached the shadow with no connection free for the flush")
	f.assertBlockedWorkers(t, poolFloorWorkers)

	release(t, hold)
	require.Eventually(t, func() bool { return f.shadowHasLabel(t, 1, flushedLabel) },
		poolFloorDeadline, poolFloorPoll, "the write is flushed once the workers free their connections")
	wg.Wait()
	require.NoError(t, runErr)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	f.assertZeroDiff(t, shape, alter)
}
