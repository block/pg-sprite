package copier_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// A chunk transaction of the earlier run can have confirmed the table lock and
// passed its guard, and still be a statement away from its insert, when the
// lock session dies and the resumed run takes the lock over: a client paused
// between two statements holds whatever locks its guard took and nothing more.
// The resumed run's first clear batch must wait for it all the same, because
// the insert it is about to send is not fenced by anything the lock session
// held, and a row it publishes after the clear read the shadow would sit above
// the watermark for good (CO-4). Here a real copier's connection is parked
// just before its chunk insert, the lock session is terminated, and the
// resumed run's fence must wait behind the parked transaction; once the
// transaction is let go its rows land, the clear removes them, and the
// earlier run, whose next transaction no longer finds its lock, stops with a
// refusal.
func TestCopierResumeWaitsForAChunkTransactionParkedBeforeItsInsert(t *testing.T) {
	f := newCopierFixture(t)
	target, earlierLock, shadow := f.prepare(t, 300)
	// An earlier run landed keys 1..150 and checkpointed watermark 150.
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.%s (id, qty)
		SELECT id, qty FROM %%s.orders WHERE id <= 150`, pgx.Identifier{shadow.ShadowTable()}.Sanitize()))

	parked := newInsertPause()
	poolConfig, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	poolConfig.ConnConfig.Tracer = parked
	earlierPool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	require.NoError(t, err)
	t.Cleanup(earlierPool.Close)

	earlier, err := copier.NewCopier(target, shadow, earlierLock, copier.NewWatermark(150), copier.Options{
		Workers:     1,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 40, MaxRows: 40},
	})
	require.NoError(t, err)
	earlierResult := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { earlierResult <- earlier.Run(t.Context(), earlierPool) })
	t.Cleanup(wg.Wait)
	t.Cleanup(parked.resume)

	const parkDeadline = 15 * time.Second
	select {
	case <-parked.parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the earlier run did not reach its first chunk insert within %s", parkDeadline)
	}

	// The earlier run's lock session dies; the resumed run takes the lock.
	f.terminateBackend(t, earlierLock.BackendPID())
	t.Cleanup(func() {
		const lockLossDeadline = 30 * time.Second
		select {
		case <-earlierLock.Done():
		case <-time.After(lockLossDeadline):
			t.Errorf("lock session did not report loss within %s", lockLossDeadline)
		}
		assert.Error(t, earlierLock.Release(context.WithoutCancel(t.Context())))
	})
	resumedLock := f.lock(t, "orders")

	resumed, err := copier.NewCopier(target, shadow, resumedLock, copier.NewWatermark(150), copier.Options{
		Workers:     1,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 40, MaxRows: 40},
	})
	require.NoError(t, err)
	resumedResult := make(chan error, 1)
	wg.Go(func() { resumedResult <- resumed.Run(t.Context(), f.pool) })

	const fenceDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		return f.waitsForShareLock(t, shadow.ShadowOID())
	}, fenceDeadline, 20*time.Millisecond, "the first clear batch should wait behind the parked chunk transaction")
	assert.Empty(t, resumed.Position().InFlight, "nothing is cut while the clear waits")

	parked.resume()
	const stopDeadline = 30 * time.Second
	select {
	case err := <-resumedResult:
		require.NoError(t, err)
	case <-time.After(stopDeadline):
		t.Fatalf("the resumed run did not return within %s of the parked transaction ending", stopDeadline)
	}
	select {
	case err := <-earlierResult:
		require.Error(t, err)
		assert.Equal(t, copier.CauseLockHeldElsewhere, copier.RefusalCauseOf(err), "the earlier run's next transaction sees the lock held by the resumed run")
	case <-time.After(stopDeadline):
		t.Fatalf("the earlier run did not return within %s of its transaction ending", stopDeadline)
	}
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(150), resumed.Position().RowsInserted, "the clear removed the parked transaction's rows, so the resumed run inserted every key above the watermark itself")
}

// insertPause is a pgx tracer that parks the first chunk insert its
// connection is about to send, after every statement before it in the chunk's
// transaction has run, until resume is called.
type insertPause struct {
	// parked is closed once an insert is held.
	parked      chan struct{}
	release     chan struct{}
	parkOnce    sync.Once
	releaseOnce sync.Once
}

func newInsertPause() *insertPause {
	return &insertPause{parked: make(chan struct{}), release: make(chan struct{})}
}

func (p *insertPause) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(data.SQL, "INSERT INTO") {
		return ctx
	}
	p.parkOnce.Do(func() {
		close(p.parked)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	})
	return ctx
}

func (p *insertPause) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// resume lets the parked insert go, once.
func (p *insertPause) resume() { p.releaseOnce.Do(func() { close(p.release) }) }

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
