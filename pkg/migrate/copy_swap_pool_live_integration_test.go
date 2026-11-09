package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The test here proves that a copy-and-swap pool at CopySwapMinConns keeps
// a connection for the catch-up's flush beyond its copy workers. Each copy
// worker holds a pool connection for as long as its chunk transaction
// lasts, and a chunk whose insert meets an uncommitted shadow row under one
// of its keys lasts until that row's transaction ends. With every worker
// held that way, the floor still has a connection for the flush, so a
// write to the source lands in the shadow while the copy stands still. A
// pool of only the workers would leave the flush queued behind them, which
// is why no entry point admits one: Run, RunDesired, and the Runner itself
// refuse a pool below the floor. While every worker is held the watermark
// does not move, so no checkpoint save is due and the floor's second spare
// connection has nothing to serve; that connection is headroom for the
// flush and the saves to proceed independently, and its count is pinned by
// schemachange's TestMinPoolConnsIsTheWorkersAndTwo. The test stands the
// workers still by holding shadow rows itself: a transaction inserts, and
// does not commit, a shadow row for one key of each held chunk, and the
// chunk's insert waits on that transaction's id.

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
	pollUntil(t, func(ctx context.Context) (bool, error) {
		var writing bool
		err := f.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_locks
				WHERE locktype = 'relation'
				  AND granted
				  AND mode = 'RowExclusiveLock'
				  AND relation = to_regclass($1)::oid)`, f.shadowName()).Scan(&writing)
		if err != nil {
			return false, fmt.Errorf("read the shadow's locks: %w", err)
		}
		return writing, nil
	}, "a transaction of the run holds the shadow for writing")
}

// pollUntil polls cond until it reports true, failing the test at
// poolFloorDeadline. A query error stops the poll at once and fails the
// test on the test's goroutine with that error, rather than waiting out
// the deadline.
func pollUntil(t *testing.T, cond func(context.Context) (bool, error), msg string) {
	t.Helper()
	var err error
	require.Eventually(t, func() bool {
		var ok bool
		ok, err = cond(t.Context())
		return ok || err != nil
	}, poolFloorDeadline, poolFloorPoll, msg)
	require.NoError(t, err, msg)
}

// startRun runs run on a goroutine the test owns and returns the wait for
// it, which fails the test if the run has not returned by
// poolFloorDeadline instead of waiting for the package timeout. The
// goroutine is also waited for at cleanup, after the holds registered
// later have been released, so it never outlives the test.
func startRun(t *testing.T, run func()) (wait func()) {
	t.Helper()
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Go(func() {
		defer close(done)
		run()
	})
	t.Cleanup(wg.Wait)
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(poolFloorDeadline):
			require.FailNow(t, "the run did not return after the holds were released")
		}
	}
}

// holdShadowRows opens a transaction on the fixture's own pool that
// inserts the source rows under keys into the shadow and leaves them
// uncommitted, and returns it for the test to roll back. A chunk whose
// insert reaches one of these keys waits on this transaction, and the
// rollback leaves nothing behind: the chunk then inserts the source row
// with no conflict. The columns are the workload table's; the key is
// written explicitly, which its identity, generated by default, allows.
// The hold first requires that no row at or above its lowest key has been
// copied yet, the premise that each held chunk is still ahead of the copy;
// a copy that outran the hold fails here by name instead of as a unique
// violation on the insert.
func (f liveFixture) holdShadowRows(t *testing.T, keys []int64) pgx.Tx {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { releaseLeftover(t, tx) })
	var copied int64
	err = tx.QueryRow(t.Context(), `
		SELECT count(*)
		FROM `+f.shadowName()+`
		WHERE id >= $1`, slices.Min(keys)).Scan(&copied)
	require.NoError(t, err)
	require.Zero(t, copied, "shadow rows copied at or above key %d before the hold opened", slices.Min(keys))
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
	pollUntil(t, func(ctx context.Context) (bool, error) {
		n, err := f.blockedWorkers(ctx)
		return n == want, err
	}, fmt.Sprintf("%d copy workers blocked on a held shadow row", want))
}

// assertBlockedWorkers asserts that exactly want copy workers are blocked
// right now.
func (f liveFixture) assertBlockedWorkers(t *testing.T, want int) {
	t.Helper()
	n, err := f.blockedWorkers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, n, "copy workers blocked on a held shadow row")
}

// shadowHasLabel returns the poll condition that the shadow row under id
// carries label.
func (f liveFixture) shadowHasLabel(id int64, label string) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		var got string
		err := f.pool.QueryRow(ctx, `
			SELECT label
			FROM `+f.shadowName()+`
			WHERE id = $1`, id).Scan(&got)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read shadow row %d: %w", id, err)
		}
		return got == label, nil
	}
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
// shadow on a connection the floor keeps beyond the workers. Releasing the
// first held chunk alone lands it and no more — the freed worker finds
// only the empty chunk above the last key and is done, and three workers
// still wait — which is the control that the holds are what stands the
// workers still. Releasing the rest lets the run finish, and
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
		v verdict.Verdict
		// runErr is set once waitForRun returns.
		runErr error
	)
	waitForRun := startRun(t, func() {
		v, runErr = migrate.Run(context.WithoutCancel(t.Context()), pool, parseOne(t, fmt.Sprintf(alter, f.table.Qualified())), opts)
	})

	f.waitForCopyWriting(t)
	holdFirst := f.holdShadowRows(t, heldKeys(firstHeldChunk, firstHeldChunk))
	holdRest := f.holdShadowRows(t, heldKeys(firstHeldChunk+1, poolFloorChunks))

	f.waitForBlockedWorkers(t, poolFloorWorkers)
	watch.waitForLanded(t, chunkKey(firstHeldChunk)-1)

	f.writeSource(t, 1, flushedLabel)
	pollUntil(t, f.shadowHasLabel(1, flushedLabel),
		"the write to a landed key is flushed into the shadow while every worker waits")
	f.assertBlockedWorkers(t, poolFloorWorkers)

	release(t, holdFirst)
	watch.waitForLanded(t, chunkKey(firstHeldChunk+1)-1)
	f.waitForBlockedWorkers(t, poolFloorWorkers-1)

	release(t, holdRest)
	waitForRun()
	require.NoError(t, runErr)
	require.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	f.assertZeroDiff(t, shape, alter)
}
