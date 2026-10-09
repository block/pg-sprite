package applier_test

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The tests below put the catch-up where nothing it waits for can arrive
// on its own — a batch a constraint refuses, a lock that is gone — and
// check what it does about it. They hold the copier's position still by
// hand (catchupSetup.positions), so the copy cannot be what breaks the
// stall.

// switchPosition is a copier position a test moves by hand.
type switchPosition struct {
	mu  sync.Mutex
	pos copier.Position
}

func (s *switchPosition) Position() copier.Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pos
}

func (s *switchPosition) set(p copier.Position) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pos = p
}

// allLanded is the position of a finished copy: every key landed, nothing
// in flight.
func allLanded() copier.Position {
	return copier.Position{Cut: copier.NewWatermark(math.MaxInt64)}
}

// oneInFlight is a finished copy's position with the chunk holding key
// alone still in flight.
func oneInFlight(t *testing.T, key int64) copier.Position {
	t.Helper()
	chunk, err := copier.NewChunk(key, key)
	require.NoError(t, err)
	p := allLanded()
	p.InFlight = []copier.Chunk{chunk}
	return p
}

// Row 1 takes row 2's unique value while row 2's chunk is in flight: row
// 1's batch collides with row 2's stale shadow row column-wise and again
// whole-row, is refused, and is requeued. Until row 2's chunk lands the
// slot is not confirmed past the swap; once it lands both rows flush
// together and the shadow converges (CO-6, ST-4).
func TestCatchupRequeuesADeferredBatchAndConverges(t *testing.T) {
	f := newCatchupFixture(t, 20)
	positions := &switchPosition{pos: oneInFlight(t, 2)}
	run := startCatchupWith(t, f, catchupSetup{positions: positions})

	q := f.table.Qualified()
	var two int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT uniq FROM `+q+` WHERE id = 2`).Scan(&two))
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = -1 WHERE id = 2`)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = $1 WHERE id = 1`, two)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))

	run.awaitStatus(t, "defer row 1's batch twice", func(s applier.Status) bool {
		return s.BatchesDeferred >= 2
	})
	stalled := run.status(t)
	assert.Equal(t, 2, stalled.Deferred, "row 2 waits on its chunk and row 1's refused batch waits with it")
	assert.Equal(t, 2, stalled.Buffered, "both rows stay buffered")
	assert.Zero(t, stalled.Applied, "nothing reaches the shadow while the batch is refused")
	assert.Less(t, stalled.Confirmed, stalled.Delivered, "the slot is not confirmed past a deferred change")

	positions.set(allLanded())
	end := f.currentWALLSN(t)
	run.awaitStatus(t, "confirm past the swap", func(s applier.Status) bool {
		return s.Confirmed >= end && s.Buffered == 0
	})
	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// The schema change narrows a uniquely indexed column, and after the copy
// has landed every key a write gives one row a value that is distinct on
// the source and collides in the shadow's type. Nothing is in flight, so no
// chunk can resolve the refusal: Run ends with it instead of requeueing the
// batch every cycle while the slot retains WAL (CO-6).
func TestCatchupEndsOnARefusalNoChunkCanResolve(t *testing.T) {
	f := newCatchupFixture(t, 20)
	q := f.table.Qualified()
	_, err := f.pool.Exec(t.Context(), `UPDATE `+q+` SET amount = id * 10`)
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), `CREATE UNIQUE INDEX ON `+q+` (amount)`)
	require.NoError(t, err)
	f.target, err = preflight.CheckCopySwap(t.Context(), f.pool, f.table.Schema, f.table.Table,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	run := startCatchupWith(t, f, catchupSetup{
		change:    `ALTER TABLE %s ALTER COLUMN amount TYPE numeric(12,0)`,
		positions: &switchPosition{pos: allLanded()},
	})

	// 20.4 differs from row 2's 20 on the source and rounds onto it in the shadow.
	_, err = f.pool.Exec(t.Context(), `UPDATE `+q+` SET amount = 20.4 WHERE id = 1`)
	require.NoError(t, err)

	err = run.wait(t)
	require.ErrorIs(t, err, applier.ErrBatchDeferred)
	assert.NotErrorIs(t, err, applier.ErrTableLockLost)
	final := run.catchup.Status()
	assert.Zero(t, final.Applied, "the refused batch never reached the shadow")
	var shadowAmount string
	shadowTable := pgx.Identifier{f.table.Schema, run.shadow.ShadowTable()}.Sanitize()
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT amount::text FROM `+shadowTable+` WHERE id = 1`).Scan(&shadowAmount))
	assert.Equal(t, "10", shadowAmount, "the shadow row keeps the copied value")
}

// The table lock is lost while the catch-up has nothing to flush. Run ends
// with the loss rather than keep consuming and confirming the slot, since
// the confirmed position is the resume point the lock protects (LK-1).
func TestCatchupEndsWhenTheTableLockIsLostOnAQuietTable(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		lockOpts:  []dbconn.TableLockOption{dbconn.WithTableLockKeepalive(200 * time.Millisecond)},
	})
	run.awaitStatus(t, "complete a cycle on the quiet table", func(s applier.Status) bool {
		return s.WALEnd > 0
	})

	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, run.lock.BackendPID()).Scan(&terminated))
	require.True(t, terminated)
	const lockLossDeadline = 10 * time.Second
	select {
	case <-run.lock.Done():
	case <-time.After(lockLossDeadline):
		t.Fatalf("lock session did not report the loss within %s", lockLossDeadline)
	}

	err := run.wait(t)
	require.ErrorIs(t, err, applier.ErrTableLockLost)
	assert.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.ErrorIs(t, err, run.lock.Err(), "the loss the session reported is the cause")
	assert.Contains(t, err.Error(), "(LK-1)")
}
