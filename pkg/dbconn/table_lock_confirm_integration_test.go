package dbconn_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// Confirm is the in-transaction check a working session runs before its
// first write. It has three outcomes: the lock session's own backend holds
// the lock; nobody does (the backend is gone and the keepalive has not yet
// noticed); or a different backend took the lock in the meantime.
func TestTableLockConfirmDistinguishesHolderGoneFromHolderElsewhere(t *testing.T) {
	url := testutil.StartPostgres(t)
	cfg := dbconn.Config{URL: url}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	held, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, held.Release(context.WithoutCancel(t.Context())))
	})
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
	})
	assert.NoError(t, held.Confirm(t.Context(), tx), "the lock session's own backend holds the lock")
	require.NoError(t, held.Release(t.Context()))

	stale := goneLock(t, cfg, pool, "app", "orders")
	err = stale.Confirm(t.Context(), tx)
	assert.ErrorIs(t, err, dbconn.ErrTableLockNotHeld)
	assert.NotErrorIs(t, err, dbconn.ErrInvariantViolation, "Confirm reports; the writing caller decides")

	rival, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, rival.Release(context.WithoutCancel(t.Context())))
	})
	err = stale.Confirm(t.Context(), tx)
	var heldErr *dbconn.TableLockHeldError
	require.ErrorAs(t, err, &heldErr)
	assert.Equal(t, rival.BackendPID(), heldErr.Holder.PID)
	assert.NotErrorIs(t, err, dbconn.ErrTableLockNotHeld)
	assert.NoError(t, rival.Confirm(t.Context(), tx), "the rival confirms its own lock on the same connection")
}

// An advisory lock is per database, but pg_locks lists every database's
// grants. A lock session dialled into another database of the same server
// holds the key there, not in the database the working transaction writes
// to, so Confirm must report the table unheld; a lookup from the working
// database must not find that session either, while a lookup from the lock's
// own database does.
func TestTableLockConfirmIgnoresTheSameKeyHeldInAnotherDatabase(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	lockURL := testutil.NewDatabase(t, serverURL)
	workURL := testutil.NewDatabase(t, serverURL)
	workPool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: workURL})
	require.NoError(t, err)
	t.Cleanup(workPool.Close)
	lockPool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: lockURL})
	require.NoError(t, err)
	t.Cleanup(lockPool.Close)

	held, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: lockURL}, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, held.Release(context.WithoutCancel(t.Context())))
	})

	tx, err := workPool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
	})
	err = held.Confirm(t.Context(), tx)
	assert.ErrorIs(t, err, dbconn.ErrTableLockNotHeld, "the lock in another database is not this database's table lock")
	var heldErr *dbconn.TableLockHeldError
	assert.NotErrorAs(t, err, &heldErr, "the other database's holder is not a rival for this table")

	_, found, err := dbconn.LookupTableLockHolder(t.Context(), workPool, "app", "orders")
	require.NoError(t, err)
	assert.False(t, found, "the working database sees no holder")
	holder, found, err := dbconn.LookupTableLockHolder(t.Context(), lockPool, "app", "orders")
	require.NoError(t, err)
	require.True(t, found, "the lock's own database sees its holder")
	assert.Equal(t, held.BackendPID(), holder.PID)
}

// The same table in two databases of one server is locked at once, as when
// two tenants' databases change app.orders together. Each lock is the
// table's lock only in its own database: from the working database, its own
// session confirms, the other database's session is not the holder there,
// and a lookup names its own session. Without the database predicate the
// lookup takes the lowest-PID grant across both databases, so whichever
// session has the higher PID fails one of the two confirmations.
func TestTableLockConfirmWhenTheSameKeyIsHeldInTwoDatabases(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	otherURL := testutil.NewDatabase(t, serverURL)
	ownURL := testutil.NewDatabase(t, serverURL)
	ownPool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: ownURL})
	require.NoError(t, err)
	t.Cleanup(ownPool.Close)

	other, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: otherURL}, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, other.Release(context.WithoutCancel(t.Context())))
	})
	own, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: ownURL}, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, own.Release(context.WithoutCancel(t.Context())))
	})

	tx, err := ownPool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
	})
	assert.NoError(t, own.Confirm(t.Context(), tx), "the own database's session confirms its lock there")
	var heldErr *dbconn.TableLockHeldError
	require.ErrorAs(t, other.Confirm(t.Context(), tx), &heldErr, "the other database's session is not the holder here")
	assert.Equal(t, own.BackendPID(), heldErr.Holder.PID)

	holder, found, err := dbconn.LookupTableLockHolder(t.Context(), ownPool, "app", "orders")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, own.BackendPID(), holder.PID)
}

// pg_locks also lists a session queued for the key: a plain
// pg_advisory_lock on a held key waits with granted = false. A waiter is
// not the holder, even when its backend has the lower PID, so the holder
// still confirms its lock and a lookup still names it. The waiter is a
// plain session blocking on the same (classid, objid) pair, as any other
// client calling pg_advisory_lock on the key would.
func TestTableLockConfirmIgnoresASessionWaitingForTheKey(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	url := testutil.NewDatabase(t, serverURL)
	cfg := dbconn.Config{URL: url}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	waiter := openRawSession(t, url)
	held, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, held.Release(context.WithoutCancel(t.Context())))
	})
	// The waiter must sort before the holder in pg_locks' PID order, or a
	// lookup that counted waiters would still name the holder by luck.
	// Backend PIDs normally grow, so the session opened first has the lower
	// one; after a PID wrap, more sessions are opened until one does.
	const maxWaiterCandidates = 64
	for opened := 1; waiter.PgConn().PID() >= held.BackendPID(); opened++ {
		require.Less(t, opened, maxWaiterCandidates, "precondition: no session among %d has a backend PID below the holder's %d", opened, held.BackendPID())
		waiter = openRawSession(t, url)
	}
	waiterPID := waiter.PgConn().PID()
	require.Less(t, waiterPID, held.BackendPID(), "precondition: the waiter's backend PID is below the holder's")

	key := held.Lock().Key()
	const waiterDeadline = 30 * time.Second
	waitCtx, cancelWait := context.WithTimeout(t.Context(), waiterDeadline)
	var wg sync.WaitGroup
	var waitErr error
	wg.Go(func() {
		_, waitErr = waiter.Exec(waitCtx, `SELECT pg_advisory_lock($1, $2)`, key.ClassID, key.ObjID)
	})
	stopWaiter := func() {
		cancelWait()
		wg.Wait()
	}
	t.Cleanup(stopWaiter)

	const waiterQueuedDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		// pg_locks reports the key's halves as oids, so a negative int32
		// half appears as its unsigned value.
		var queued bool
		require.NoError(t, pool.QueryRow(t.Context(), `
			SELECT EXISTS (
			  SELECT 1
			    FROM pg_locks
			   WHERE locktype = 'advisory'
			     AND pid = $1
			     AND NOT granted
			     AND classid::bigint = $2
			     AND objid::bigint = $3
			     AND objsubid = 2)`,
			waiterPID, uint32(key.ClassID), uint32(key.ObjID)).Scan(&queued))
		return queued
	}, waiterQueuedDeadline, 20*time.Millisecond, "the waiter should queue behind the holder in pg_locks")

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
	})
	assert.NoError(t, held.Confirm(t.Context(), tx), "the waiter is not a rival holder")
	holder, found, err := dbconn.LookupTableLockHolder(t.Context(), pool, "app", "orders")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, held.BackendPID(), holder.PID)

	stopWaiter()
	assert.ErrorIs(t, waitErr, context.Canceled, "the waiter was still queued when the test stopped it")
}

// openRawSession opens a plain session to url with no pg-sprite preparation,
// standing in for a client outside pg-sprite.
func openRawSession(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if conn.IsClosed() {
			return
		}
		assert.NoError(t, conn.Close(ctx))
	})
	return conn
}

// goneLock acquires the table lock and then terminates the session's
// backend, so the server no longer grants the lock while the session still
// believes it holds it. The keepalive is an hour away, so only an
// in-transaction confirmation can catch the loss during the test. The
// cleanup releases without waiting for the keepalive to notice;
// TestTableLockKeepaliveLoss covers that detection.
func goneLock(t *testing.T, cfg dbconn.Config, pool *pgxpool.Pool, schema, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), cfg, schema, table, dbconn.WithTableLockKeepalive(time.Hour))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.Error(t, lock.Release(context.WithoutCancel(t.Context())), "releasing a session whose backend is gone fails")
	})
	var terminated bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, lock.BackendPID()).Scan(&terminated))
	require.True(t, terminated)
	const backendExitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var alive bool
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, lock.BackendPID()).Scan(&alive))
		return !alive
	}, backendExitDeadline, 50*time.Millisecond, "terminated backend should leave pg_stat_activity")
	require.NoError(t, lock.Err(), "the keepalive has not yet noticed the loss; the in-transaction check must")
	return lock
}
