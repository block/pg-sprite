package dbconn_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

func TestTableLockTwoInstanceRefusal(t *testing.T) {
	url := testutil.StartPostgres(t)
	cfg := dbconn.Config{URL: url}

	first, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, first.Release(context.WithoutCancel(t.Context())))
	})

	proof := first.Lock()
	assert.Equal(t, "app", proof.Schema())
	assert.Equal(t, "orders", proof.Table())

	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// The key is the form an operator can reproduce from a runbook: the
	// pg-sprite classid and hashtext over the quoted qualified name.
	var wantObjID int32
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT hashtext(quote_ident($1) || '.' || quote_ident($2))", "app", "orders").Scan(&wantObjID))
	assert.Equal(t, dbconn.TableLockKey{ClassID: dbconn.TableLockClassID, ObjID: wantObjID}, proof.Key())

	second, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.Error(t, err)
	assert.Nil(t, second)
	var heldErr *dbconn.TableLockHeldError
	require.ErrorAs(t, err, &heldErr)
	assert.Equal(t, "app", heldErr.Schema)
	assert.Equal(t, "orders", heldErr.Table)
	// The refusal names the holder, so an operator can tell another
	// pg-sprite instance from a colliding key without a second query.
	assert.Equal(t, first.BackendPID(), heldErr.Holder.PID)
	assert.Equal(t, "pg-sprite", heldErr.Holder.ApplicationName)
	assert.False(t, heldErr.Holder.BackendStart.IsZero())

	holder, found, err := dbconn.LookupTableLockHolder(t.Context(), pool, "app", "orders")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, first.BackendPID(), holder.PID)

	different, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "customers")
	require.NoError(t, err)
	require.NoError(t, different.Release(t.Context()))
	require.NoError(t, first.Release(t.Context()))

	_, found, err = dbconn.LookupTableLockHolder(t.Context(), pool, "app", "orders")
	require.NoError(t, err)
	assert.False(t, found, "a released lock has no holder to report")

	second, err = dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.NoError(t, err)
	require.NoError(t, second.Release(t.Context()))
}

func TestTableLockKeepaliveLoss(t *testing.T) {
	url := testutil.StartPostgres(t)
	cfg := dbconn.Config{URL: url}
	session, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "events",
		dbconn.WithTableLockKeepalive(50*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.Error(t, session.Release(context.WithoutCancel(t.Context())))
	})

	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var terminated bool
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_terminate_backend($1)", session.BackendPID()).Scan(&terminated))
	require.True(t, terminated)

	const lockLossDeadline = 10 * time.Second
	select {
	case <-session.Done():
		require.Error(t, session.Err())
		assert.NotErrorIs(t, session.Err(), dbconn.ErrInvariantViolation,
			"a dead session is a keepalive failure, not a lock another session took")
	case <-time.After(lockLossDeadline):
		t.Fatalf("table lock loss was not detected within %s", lockLossDeadline)
	}

	replacement, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "events")
	require.NoError(t, err)
	require.NoError(t, replacement.Release(t.Context()))
	assert.False(t, errors.Is(session.Err(), context.Canceled))
}

// The acquisition context bounds only the dial and the lock attempt: once
// the lock is held, cancelling it must not end the session. The keepalive is
// short so several confirmations run on the cancelled context before the
// assertions read the lock back.
func TestTableLockAcquireContextDoesNotOwnSession(t *testing.T) {
	url := testutil.StartPostgres(t)
	acquireCtx, cancelAcquire := context.WithCancel(t.Context())
	session, err := dbconn.AcquireTableLock(acquireCtx, dbconn.Config{URL: url}, "app", "long_change",
		dbconn.WithTableLockKeepalive(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, session.Release(context.WithoutCancel(t.Context())))
	})
	cancelAcquire()

	const keepaliveTicks = 500 * time.Millisecond
	require.Never(t, func() bool {
		select {
		case <-session.Done():
			return true
		default:
			return false
		}
	}, keepaliveTicks, 50*time.Millisecond, "table lock session ended when its acquisition context was cancelled")

	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var held bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_locks
		 WHERE locktype = 'advisory' AND granted AND objsubid = 2 AND pid = $1
	)`, session.BackendPID()).Scan(&held))
	assert.True(t, held)
	assert.NoError(t, session.Err())
}

// Bind turns the session's end into cancellation of the work it protects:
// a lost lock cancels with the loss as the cause, and Release cancels with
// context.Canceled, so a consumer cannot keep running unprotected by
// forgetting to watch Done.
func TestTableLockBindCancelsOnLoss(t *testing.T) {
	url := testutil.StartPostgres(t)
	cfg := dbconn.Config{URL: url}
	session, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "bound_events",
		dbconn.WithTableLockKeepalive(50*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.Error(t, session.Release(context.WithoutCancel(t.Context())))
	})
	work, stop := session.Bind(t.Context())
	defer stop()

	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var terminated bool
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_terminate_backend($1)", session.BackendPID()).Scan(&terminated))
	require.True(t, terminated)

	const lockLossDeadline = 10 * time.Second
	select {
	case <-work.Done():
		require.Error(t, session.Err())
		assert.Equal(t, session.Err(), context.Cause(work))
	case <-time.After(lockLossDeadline):
		t.Fatalf("bound context was not cancelled within %s of lock loss", lockLossDeadline)
	}
}

func TestTableLockBindCancelsOnRelease(t *testing.T) {
	url := testutil.StartPostgres(t)
	session, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: url}, "app", "bound_orders")
	require.NoError(t, err)
	work, stop := session.Bind(t.Context())
	defer stop()

	require.NoError(t, session.Release(t.Context()))
	<-work.Done()
	assert.ErrorIs(t, context.Cause(work), context.Canceled)
	assert.NoError(t, session.Err())
}

func TestTableLockDoneClosesOnRelease(t *testing.T) {
	url := testutil.StartPostgres(t)
	session, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: url}, "app", "released_orders")
	require.NoError(t, err)
	require.NoError(t, session.Release(t.Context()))

	select {
	case <-session.Done():
		assert.NoError(t, session.Err())
	default:
		t.Fatal("table lock Done remained open after Release")
	}
}
