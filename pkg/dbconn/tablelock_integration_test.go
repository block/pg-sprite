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

// lockPollDeadline bounds every wait for the keepalive to notice something.
const lockPollDeadline = 30 * time.Second

// instance is one pg-sprite process's view of the database: its own pool,
// its own sessions. Two of them is the shape LK-1 is about.
func instance(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// releaseLock releases a lock inside a test body, where the deferred
// cleanup's context is still live.
func releaseLock(t *testing.T, lock *dbconn.TableLock) {
	t.Helper()
	require.NoError(t, lock.Release(t.Context()))
}

// Two instances cannot change one table at the same time: the second to ask
// is refused outright rather than proceeding alongside the first, and the
// refusal is typed so a front door can turn it into a retryable answer. The
// lock is released when the first instance finishes, so the second's next
// attempt succeeds — exclusion, not a permanent claim.
func TestTableLockExcludesASecondInstance(t *testing.T) {
	url := testutil.StartPostgres(t)
	first, second := instance(t, url), instance(t, url)
	schema := testutil.NewSchema(t, first)

	held, err := dbconn.AcquireTableLock(t.Context(), first, schema, "orders", dbconn.TableLockOptions{})
	require.NoError(t, err)

	_, err = dbconn.AcquireTableLock(t.Context(), second, schema, "orders", dbconn.TableLockOptions{})
	require.ErrorIs(t, err, dbconn.ErrTableLocked)

	releaseLock(t, held)

	after, err := dbconn.AcquireTableLock(t.Context(), second, schema, "orders", dbconn.TableLockOptions{})
	require.NoError(t, err, "the lock must be free once the holder released it")
	releaseLock(t, after)
}

// The lock serializes per table, not per database: two instances changing
// different tables never wait on each other, so one long change cannot stall
// unrelated work.
func TestTableLockDoesNotContendAcrossTables(t *testing.T) {
	url := testutil.StartPostgres(t)
	first, second := instance(t, url), instance(t, url)
	schema := testutil.NewSchema(t, first)

	orders, err := dbconn.AcquireTableLock(t.Context(), first, schema, "orders", dbconn.TableLockOptions{})
	require.NoError(t, err)
	defer releaseLock(t, orders)

	customers, err := dbconn.AcquireTableLock(t.Context(), second, schema, "customers", dbconn.TableLockOptions{})
	require.NoError(t, err)
	releaseLock(t, customers)
}

// A lock is only as good as the session holding it. When that session dies —
// an idle-session reaper, a failover, an operator's pg_terminate_backend —
// the keepalive discovers it rather than believing the lock is still held,
// and the discovery is fail-closed: the context guarding the change is
// cancelled, so work under it stops instead of continuing unprotected, and
// the cause says the lock was lost.
func TestTableLockGuardIsCancelledWhenTheSessionDies(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool := instance(t, url)
	schema := testutil.NewSchema(t, pool)

	lock, err := dbconn.AcquireTableLock(t.Context(), pool, schema, "orders",
		dbconn.TableLockOptions{KeepaliveInterval: 50 * time.Millisecond})
	require.NoError(t, err)
	defer func() { _ = lock.Release(context.WithoutCancel(t.Context())) }()

	guarded, cancel, err := lock.Guard(t.Context())
	require.NoError(t, err)
	defer cancel()
	require.NoError(t, guarded.Err(), "the guard must be live while the lock is held")

	_, err = pool.Exec(t.Context(), "SELECT pg_terminate_backend($1)", lock.BackendPID())
	require.NoError(t, err)

	select {
	case <-guarded.Done():
		require.ErrorIs(t, context.Cause(guarded), dbconn.ErrLockLost)
	case <-time.After(lockPollDeadline):
		t.Fatal("the guard was not cancelled after the lock's session was terminated")
	}
	require.ErrorIs(t, lock.LostCause(), dbconn.ErrLockLost)
}

// Losing the session frees the lock on the server, so another instance can
// take it. The instance that lost it must not go on believing it holds it:
// its own confirmation is what tells it, which is why the confirmation reads
// the catalog rather than the client's memory of what it took.
func TestTableLockConfirmDetectsALostLock(t *testing.T) {
	url := testutil.StartPostgres(t)
	first, second := instance(t, url), instance(t, url)
	schema := testutil.NewSchema(t, first)

	lock, err := dbconn.AcquireTableLock(t.Context(), first, schema, "orders",
		dbconn.TableLockOptions{KeepaliveInterval: time.Hour})
	require.NoError(t, err)
	defer func() { _ = lock.Release(context.WithoutCancel(t.Context())) }()
	require.NoError(t, lock.Confirm(t.Context()))

	_, err = first.Exec(t.Context(), "SELECT pg_terminate_backend($1)", lock.BackendPID())
	require.NoError(t, err)

	require.ErrorIs(t, lock.Confirm(t.Context()), dbconn.ErrLockLost,
		"a confirmation must read the lock's state, not the client's memory of taking it")

	// The other instance is free to proceed, which is exactly why the first
	// one must stop.
	taken, err := dbconn.AcquireTableLock(t.Context(), second, schema, "orders", dbconn.TableLockOptions{})
	require.NoError(t, err)
	releaseLock(t, taken)
}

// A guard is refused outright once the lock is known lost: a mutating
// operation that asks for one after that gets an error, never a context it
// could run under.
func TestTableLockGuardRefusesAfterTheLockIsLost(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool := instance(t, url)
	schema := testutil.NewSchema(t, pool)

	lock, err := dbconn.AcquireTableLock(t.Context(), pool, schema, "orders",
		dbconn.TableLockOptions{KeepaliveInterval: time.Hour})
	require.NoError(t, err)
	defer func() { _ = lock.Release(context.WithoutCancel(t.Context())) }()

	_, err = pool.Exec(t.Context(), "SELECT pg_terminate_backend($1)", lock.BackendPID())
	require.NoError(t, err)
	require.Error(t, lock.Confirm(t.Context()))

	_, _, err = lock.Guard(t.Context())
	require.ErrorIs(t, err, dbconn.ErrLockLost)
}

// Releasing twice is not an error and does not double-unlock: the second
// release is a no-op, so a caller with both a deferred release and an
// explicit one cannot free a lock a later acquisition took.
func TestTableLockReleaseIsIdempotent(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool := instance(t, url)
	schema := testutil.NewSchema(t, pool)

	lock, err := dbconn.AcquireTableLock(t.Context(), pool, schema, "orders", dbconn.TableLockOptions{})
	require.NoError(t, err)
	require.NoError(t, lock.Release(t.Context()))
	require.NoError(t, lock.Release(t.Context()))
}

// The lock lives on its own connection, so it does not consume the pool the
// change itself runs on: an engine holding the lock still has every
// connection it was configured with, including the two a concurrent index
// build needs.
func TestTableLockDoesNotConsumeTheCallerPool(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url, MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	lock, err := dbconn.AcquireTableLock(t.Context(), pool, schema, "orders", dbconn.TableLockOptions{})
	require.NoError(t, err)
	defer releaseLock(t, lock)

	first, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer first.Release()
	second, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer second.Release()

	var one int
	require.NoError(t, second.QueryRow(t.Context(), "SELECT 1").Scan(&one))
	assert.Equal(t, 1, one)
}

// An acquisition that cannot prove its connection keeps one server session
// takes no lock at all, so nothing is left behind on a backend the caller
// cannot reach. Against a direct connection the proof passes, which is the
// case the whole contract is written for.
func TestProveSessionAffinityPassesOnADirectConnection(t *testing.T) {
	pool := instance(t, testutil.StartPostgres(t))
	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer conn.Release()

	require.NoError(t, dbconn.ProveSessionAffinity(t.Context(), conn, pool))
}
