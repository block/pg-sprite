package dbconn_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// A transaction-mode pooler gives a client no stable server session, so a
// session-scoped lock taken on it is not exclusive: the pooler can hand the
// same backend to a second client, which then re-enters the lock it already
// holds. The pool refuses such an endpoint outright; the lock that LK-1
// rests on must refuse it too, and must refuse before taking anything.
func TestAcquireTableLockRefusesAConnectionWithoutSessionAffinity(t *testing.T) {
	pooledURL := testutil.StartPostgresBehindPgBouncer(t, testutil.TransactionPooling)
	cfg := dbconn.Config{URL: pooledURL}

	session, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.ErrorIs(t, err, dbconn.ErrNoSessionAffinity)
	assert.Contains(t, err.Error(), "session-mode endpoint", "the refusal must name what to change")
	require.Nil(t, session)
}

// A session-mode pooler keeps one backend per client connection, which is
// the endpoint the refusal above tells an operator to move to, so the lock
// must work through it.
func TestAcquireTableLockAcceptsSessionPooling(t *testing.T) {
	pooledURL := testutil.StartPostgresBehindPgBouncer(t, testutil.SessionPooling)
	cfg := dbconn.Config{URL: pooledURL}

	first, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, first.Release(context.WithoutCancel(t.Context())))
	})

	second, err := dbconn.AcquireTableLock(t.Context(), cfg, "app", "orders")
	var heldErr *dbconn.TableLockHeldError
	require.ErrorAs(t, err, &heldErr)
	assert.Nil(t, second)
	assert.Equal(t, first.BackendPID(), heldErr.Holder.PID)
}
