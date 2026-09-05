package dbconn_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// A PostgreSQL advisory lock is exclusive only while a client connection
// keeps one server session. Behind a transaction-mode pooler it does not:
// the pooler hands the backend back at the end of the statement that took
// the lock, and a second client connection reaching that backend takes the
// same lock while the first still believes it holds it. This is the
// mechanism that would remove mutual exclusion from LK-1 without any error
// being raised anywhere, so it is pinned directly rather than only through
// the guard that detects it.
func TestAdvisoryLocksLoseExclusionBehindTransactionPooling(t *testing.T) {
	pooledURL, _ := testutil.StartPostgresBehindPgBouncer(t, testutil.TransactionPooling)
	pool := instance(t, pooledURL)
	const key = int64(918273645)

	first, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer first.Release()
	var taken bool
	require.NoError(t, first.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)", key).Scan(&taken))
	require.True(t, taken, "the first connection should take the lock")

	second, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer second.Release()
	require.NoError(t, second.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)", key).Scan(&taken))

	assert.True(t, taken,
		"two pooled connections both holding one advisory lock is the lost exclusion this guard exists to refuse")
}

// An engine that cannot hold a session-scoped lock must not pretend it can.
// Acquiring the table lock over a transaction-mode pooler refuses with a
// typed error and takes no lock, so a caller fails closed instead of running
// a schema change with exclusion it does not have.
func TestAcquireTableLockRefusesTransactionPooling(t *testing.T) {
	pooledURL, _ := testutil.StartPostgresBehindPgBouncer(t, testutil.TransactionPooling)
	pool := instance(t, pooledURL)
	schema := testutil.NewSchema(t, pool)

	_, err := dbconn.AcquireTableLock(t.Context(), pool, schema, "orders", dbconn.TableLockOptions{})

	require.ErrorIs(t, err, dbconn.ErrNoSessionAffinity)
}

// The refusal is keyed on the property the lock needs, not on the presence
// of a pooler: session pooling gives a client connection its own server
// session, which is all a session-scoped lock requires, so a pooled
// connection string is not refused for being pooled. The lock taken through
// it excludes a second instance exactly as a direct one does.
func TestAcquireTableLockAcceptsSessionPooling(t *testing.T) {
	pooledURL, _ := testutil.StartPostgresBehindPgBouncer(t, testutil.SessionPooling)
	first, second := instance(t, pooledURL), instance(t, pooledURL)
	schema := testutil.NewSchema(t, first)

	lock, err := dbconn.AcquireTableLock(t.Context(), first, schema, "orders", dbconn.TableLockOptions{})
	require.NoError(t, err, "session pooling keeps one backend per client connection")
	defer releaseLock(t, lock)

	_, err = dbconn.AcquireTableLock(t.Context(), second, schema, "orders", dbconn.TableLockOptions{})
	require.ErrorIs(t, err, dbconn.ErrTableLocked, "the lock must still exclude a second instance")
}
