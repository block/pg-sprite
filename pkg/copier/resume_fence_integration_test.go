package copier_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// A chunk transaction of the earlier run can still be committing when the
// resumed run starts — a commit waiting on a synchronous standby holds its
// locks and keeps its rows invisible until the standby answers. A clear that
// ran ahead of it would miss the row, the straggler would then publish it
// above the watermark, and the resumed copy's never-overwriting insert would
// keep that stale image for good (CO-4). The resumed run's first clear batch
// therefore waits behind the straggler's ROW EXCLUSIVE lock on the shadow,
// cuts nothing meanwhile, and removes the straggler's row once it lands.
// The straggler here is an insert left uncommitted: it holds the same lock,
// and its rows are invisible for the same reason, as one mid-commit.
func TestCopierResumeWaitsForAStragglingChunkTransaction(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepare(t, 300)
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()

	straggler, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	// Redundant safety closer: the test commits, after which Rollback
	// returns the guaranteed ErrTxClosed.
	t.Cleanup(func() { _ = straggler.Rollback(context.WithoutCancel(t.Context())) })
	_, err = straggler.Exec(t.Context(), "INSERT INTO "+shadowName+" (id, qty) VALUES (200, 0)")
	require.NoError(t, err)

	c, err := copier.NewCopier(target, shadow, lock, copier.NewWatermark(150), copier.Options{
		Workers:     1,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 40, MaxRows: 40},
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	const fenceDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		return f.waitsForShareLock(t, shadow.ShadowOID())
	}, fenceDeadline, 20*time.Millisecond, "the first clear batch should wait behind the straggler's lock")
	pos := c.Position()
	assert.Empty(t, pos.InFlight, "nothing is cut while the clear waits")
	assert.Equal(t, copier.NewWatermark(150), pos.Cut, "the frontier stays at the watermark while the clear waits")
	require.NoError(t, straggler.Commit(t.Context()))

	const stopDeadline = 30 * time.Second
	select {
	case err := <-results:
		require.NoError(t, err)
	case <-time.After(stopDeadline):
		t.Fatalf("Run did not return within %s of the straggler committing", stopDeadline)
	}

	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id = 200 AND qty = 0"), "the straggler's row did not survive the clear")
	assert.Equal(t, int64(1), f.count(t, shadow.ShadowTable(), "id = 200 AND qty = 200"), "the key was read from the source")
	assert.Equal(t, int64(150), f.count(t, shadow.ShadowTable(), "id > 150 AND qty = id"), "every key above the watermark was read from the source")
	assert.Equal(t, int64(150), c.Position().RowsInserted)
}

// waitsForShareLock reports whether some backend is waiting for a SHARE MODE
// lock on relation oid: the fence, blocked behind a straggler.
func (f copierFixture) waitsForShareLock(t *testing.T, oid uint32) bool {
	t.Helper()
	return f.waitsForLock(t, oid, "ShareLock")
}

// waitsForLock reports whether some backend is waiting for a lock of mode
// (as pg_locks spells it) on relation oid.
func (f copierFixture) waitsForLock(t *testing.T, oid uint32, mode string) bool {
	t.Helper()
	var waiting bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype = 'relation' AND relation = $1 AND mode = $2 AND NOT granted
		)`, oid, mode).Scan(&waiting))
	return waiting
}
