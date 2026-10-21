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
	"github.com/block/pg-sprite/pkg/dbconn"
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
		return f.waitsForFenceLock(t, shadow.ShadowOID())
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
	earlierPool := f.pausedPool(t, parked)

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
		return f.waitsForFenceLock(t, shadow.ShadowOID())
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

// A chunk transaction of the earlier run that has locked the source but not
// yet the shadow when the lock session dies holds nothing the resumed run's
// fence waits on, so the fence must not have to: the transaction confirms
// the table lock only after it takes the shadow lock, and by then the lock
// has passed to the resumed run, so it is refused before it writes. Were the
// confirmation to come first, this transaction would have confirmed the lock
// while it still held it, then taken the shadow lock after the resumed
// run's clear had committed, and inserted a chunk above the watermark that
// the resumed run's never-overwriting insert keeps for good (CO-4). Here the
// earlier run's connection is parked at its shadow lock statement, its lock
// session's backend is terminated under a client that will not notice, the
// resumed run clears and is parked at its first insert, and only then is
// the earlier transaction let go.
func TestCopierResumeRefusesAChunkTransactionParkedBeforeItsShadowLock(t *testing.T) {
	f := newCopierFixture(t)
	f.createOrders(t, 300)
	target := f.prove(t, "orders")
	earlierLock := f.stalledClientLock(t, "orders")
	shadow := f.build(t, earlierLock, target)
	// An earlier run landed keys 1..150 and checkpointed watermark 150.
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.%s (id, qty)
		SELECT id, qty FROM %%s.orders WHERE id <= 150`, pgx.Identifier{shadow.ShadowTable()}.Sanitize()))

	earlierParked := newShadowWriteLockPause()
	earlierPool := f.pausedPool(t, earlierParked)
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
	t.Cleanup(earlierParked.resume)

	const parkDeadline = 15 * time.Second
	select {
	case <-earlierParked.parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the earlier run did not reach its first chunk's shadow lock within %s", parkDeadline)
	}

	// The earlier run's lock session dies unnoticed; the resumed run takes
	// the lock, clears, and is held just before its first insert.
	f.terminateBackend(t, earlierLock.BackendPID())
	resumedLock := f.lock(t, "orders")
	resumedParked := newInsertPause()
	resumedPool := f.pausedPool(t, resumedParked)
	resumed, err := copier.NewCopier(target, shadow, resumedLock, copier.NewWatermark(150), copier.Options{
		Workers:     1,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 40, MaxRows: 40},
	})
	require.NoError(t, err)
	resumedResult := make(chan error, 1)
	wg.Go(func() { resumedResult <- resumed.Run(t.Context(), resumedPool) })
	t.Cleanup(resumedParked.resume)
	select {
	case <-resumedParked.parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the resumed run did not clear and reach its first chunk insert within %s", parkDeadline)
	}

	// The earlier transaction goes on after the clear committed: it takes
	// its shadow lock, then finds the table lock held by the resumed run.
	earlierParked.resume()
	const stopDeadline = 30 * time.Second
	err = awaitRun(t, earlierResult, stopDeadline, "the earlier run")
	require.Error(t, err)
	assert.Equal(t, copier.CauseLockHeldElsewhere, copier.RefusalCauseOf(err), "the earlier run's chunk transaction confirms the lock after it takes the shadow lock, and is refused")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id > 150"), "no row of the earlier run lands above the watermark behind the clear")

	resumedParked.resume()
	require.NoError(t, awaitRun(t, resumedResult, stopDeadline, "the resumed run"))
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(150), resumed.Position().RowsInserted, "the resumed run inserted every key above the watermark itself")
}

// An earlier run can be stalled inside its own resume clear — its first
// batch's guard done and the fence lock held, its delete not yet sent — when
// its lock session dies and a resumed run starts clearing too. The resumed
// run's fence then waits behind the earlier run's fence. Both hold the one
// lock mode their guard took as its first on the shadow and neither asks for
// a stronger one afterwards, so the server has nothing to break: the resumed
// fence is granted once the earlier batch commits, and the earlier run's next
// transaction finds its lock gone and is refused. A fence that strengthened
// a lock it already held would instead have each transaction waiting on the
// other, and the server would abort one of them — possibly the resumed run,
// with no refusal cause (CO-4).
func TestCopierResumeQueuesBehindAnEarlierRunStalledInsideItsClear(t *testing.T) {
	f := newCopierFixture(t)
	f.createOrders(t, 300)
	target := f.prove(t, "orders")
	earlierLock := f.stalledClientLock(t, "orders")
	shadow := f.build(t, earlierLock, target)
	// An earlier run landed keys 1..150 and checkpointed watermark 150.
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.%s (id, qty)
		SELECT id, qty FROM %%s.orders WHERE id <= 150`, pgx.Identifier{shadow.ShadowTable()}.Sanitize()))

	earlierParked := newClearDeletePause()
	earlierPool := f.pausedPool(t, earlierParked)
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
	t.Cleanup(earlierParked.resume)

	const parkDeadline = 15 * time.Second
	select {
	case <-earlierParked.parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the earlier run did not reach its first clear delete within %s", parkDeadline)
	}

	// The earlier run's lock session dies unnoticed; the resumed run takes
	// the lock and its fence queues behind the earlier run's.
	f.terminateBackend(t, earlierLock.BackendPID())
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
		return f.waitsForFenceLock(t, shadow.ShadowOID())
	}, fenceDeadline, 20*time.Millisecond, "the resumed run's fence should wait behind the earlier run's fence")
	assert.Empty(t, resumed.Position().InFlight, "nothing is cut while the clear waits")

	earlierParked.resume()
	const stopDeadline = 30 * time.Second
	require.NoError(t, awaitRun(t, resumedResult, stopDeadline, "the resumed run"), "the resumed run is granted its fence once the earlier batch ends, not aborted")
	err = awaitRun(t, earlierResult, stopDeadline, "the earlier run")
	require.Error(t, err)
	assert.Equal(t, copier.CauseLockHeldElsewhere, copier.RefusalCauseOf(err), "the earlier run's next transaction sees the lock held by the resumed run")
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(150), resumed.Position().RowsInserted, "the resumed run inserted every key above the watermark itself")
}

// statementPause is a pgx tracer that parks the first statement its
// connection is about to send for which match reports true, after every
// statement before it in the transaction has run, until resume is called.
type statementPause struct {
	match func(sql string) bool
	// parked is closed once a statement is held.
	parked      chan struct{}
	release     chan struct{}
	parkOnce    sync.Once
	releaseOnce sync.Once
}

func newStatementPause(match func(sql string) bool) *statementPause {
	return &statementPause{match: match, parked: make(chan struct{}), release: make(chan struct{})}
}

// newInsertPause parks the first chunk insert.
func newInsertPause() *statementPause {
	return newStatementPause(func(sql string) bool { return strings.HasPrefix(sql, "INSERT INTO") })
}

// newShadowWriteLockPause parks the first ROW EXCLUSIVE lock on the shadow:
// a chunk transaction's first statement on it, after it has locked the
// source and before it confirms the table lock.
func newShadowWriteLockPause() *statementPause {
	return newStatementPause(func(sql string) bool {
		return strings.HasPrefix(sql, "LOCK TABLE") && strings.HasSuffix(sql, "IN ROW EXCLUSIVE MODE")
	})
}

// newClearDeletePause parks the first clear batch's delete, after its guard
// has taken the fence lock on the shadow.
func newClearDeletePause() *statementPause {
	return newStatementPause(func(sql string) bool { return strings.HasPrefix(sql, "DELETE FROM") })
}

func (p *statementPause) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !p.match(data.SQL) {
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

func (p *statementPause) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// resume lets the parked statement go, once.
func (p *statementPause) resume() { p.releaseOnce.Do(func() { close(p.release) }) }

// pausedPool is a pool for the schema under test whose connections carry
// pause.
func (f copierFixture) pausedPool(t *testing.T, pause *statementPause) *pgxpool.Pool {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	poolConfig.ConnConfig.Tracer = pause
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// stalledClientLock is a lock session whose client will not notice its
// backend's loss for the rest of the test: its keepalive is an hour away, so
// once the backend is terminated only an in-transaction confirmation can
// catch the loss. The test terminates the backend itself; Release then
// fails on the dead connection.
func (f copierFixture) stalledClientLock(t *testing.T, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table, dbconn.WithTableLockKeepalive(time.Hour))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.Error(t, lock.Release(context.WithoutCancel(t.Context())), "the backend was terminated, so the release cannot reach it")
	})
	return lock
}

// awaitRun waits for a copier's Run to return within deadline and hands back
// its error.
func awaitRun(t *testing.T, results <-chan error, deadline time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(deadline):
		t.Fatalf("%s did not return within %s", what, deadline)
		return nil
	}
}

// waitsForFenceLock reports whether some backend is waiting for a SHARE ROW
// EXCLUSIVE lock on relation oid: the fence, blocked behind a straggler or
// behind an earlier run's fence.
func (f copierFixture) waitsForFenceLock(t *testing.T, oid uint32) bool {
	t.Helper()
	return f.waitsForLock(t, oid, "ShareRowExclusiveLock")
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
