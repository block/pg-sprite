package dbconn_test

import (
	"context"
	"testing"
	"time"

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

// goneLock acquires the table lock and then terminates the session's
// backend, so the server no longer grants the lock while the session still
// believes it holds it. The cleanup waits for the keepalive to notice the
// loss before releasing.
func goneLock(t *testing.T, cfg dbconn.Config, pool *pgxpool.Pool, schema, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), cfg, schema, table)
	require.NoError(t, err)
	t.Cleanup(func() {
		const lockLossDeadline = 30 * time.Second
		select {
		case <-lock.Done():
		case <-time.After(lockLossDeadline):
			t.Errorf("lock session did not report loss within %s", lockLossDeadline)
		}
		assert.Error(t, lock.Release(context.WithoutCancel(t.Context())))
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
