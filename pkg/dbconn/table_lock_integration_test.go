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
	var wantKey int64
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT hashtext($1)::bigint", "app.orders").Scan(&wantKey))
	assert.Equal(t, wantKey, proof.Key())

	second, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.Error(t, err)
	assert.Nil(t, second)
	var heldErr *dbconn.TableLockHeldError
	require.ErrorAs(t, err, &heldErr)
	assert.Equal(t, "app", heldErr.Schema)
	assert.Equal(t, "orders", heldErr.Table)

	different, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "customers")
	require.NoError(t, err)
	require.NoError(t, different.Release(t.Context()))
	require.NoError(t, first.Release(t.Context()))

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
	case <-time.After(lockLossDeadline):
		t.Fatalf("table lock loss was not detected within %s", lockLossDeadline)
	}

	replacement, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "events")
	require.NoError(t, err)
	require.NoError(t, replacement.Release(t.Context()))
	assert.False(t, errors.Is(session.Err(), context.Canceled))
}

func TestTableLockAcquireContextDoesNotOwnSession(t *testing.T) {
	url := testutil.StartPostgres(t)
	acquireCtx, cancelAcquire := context.WithCancel(t.Context())
	session, err := dbconn.AcquireTableLock(acquireCtx, dbconn.Config{URL: url}, "app", "long_change")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, session.Release(context.WithoutCancel(t.Context())))
	})
	cancelAcquire()

	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var held bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_locks
		 WHERE locktype = 'advisory' AND granted AND pid = $1
	)`, session.BackendPID()).Scan(&held))
	assert.True(t, held)
	select {
	case <-session.Done():
		t.Fatal("table lock session ended when its acquisition context was cancelled")
	default:
	}
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
