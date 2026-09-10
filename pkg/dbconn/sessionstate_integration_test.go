package dbconn_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// The engine's execution bounds are session settings: the pool sends
// lock_timeout and statement_timeout on every connection and every
// statement then relies on them. On a connection that keeps its session,
// they are there.
func TestPoolCarriesItsSessionTimeouts(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:              testutil.StartPostgres(t),
		LockTimeout:      1500 * time.Millisecond,
		StatementTimeout: 7 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	assert.Equal(t, int64(1500), sessionSetting(t, pool, "lock_timeout"))
	assert.Equal(t, int64(7000), sessionSetting(t, pool, "statement_timeout"))
}

// A transaction-mode pooler gives a client no stable server session, so a
// bound set on one statement's backend may simply not be there for the next
// one. The engine would run unbounded while believing it was bounded: an
// ALTER that queues would sit at the head of the lock queue indefinitely,
// blocking every reader and writer behind it. That is LK-2 gone, silently,
// so the pool refuses to open at all.
//
// Reading the bounds back cannot catch this on its own — an idle pooler
// hands the same backend out again and reports them present — so the
// refusal rests on the affinity proof, which forces the rebind.
func TestNewPoolRefusesAConnectionThatDiscardsSessionTimeouts(t *testing.T) {
	pooledURL := testutil.StartPostgresBehindPgBouncer(t, testutil.TransactionPooling)

	_, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: pooledURL})

	require.ErrorIs(t, err, dbconn.ErrNoSessionAffinity)
	assert.Contains(t, err.Error(), "session-mode endpoint",
		"the refusal must name what to change")
}

// The refusal is keyed on whether the session keeps the settings, not on
// whether a pooler is present. Session pooling gives a client connection its
// own backend for the connection's lifetime, which carries the startup
// parameters, so a pooled connection string is not refused for being pooled
// — this is what lets pg-sprite run against a hosted platform whose
// session-mode endpoint is the same host on a different port.
func TestNewPoolAcceptsSessionPooling(t *testing.T) {
	pooledURL := testutil.StartPostgresBehindPgBouncer(t, testutil.SessionPooling)

	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: pooledURL, LockTimeout: 2 * time.Second})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	assert.Equal(t, int64(2000), sessionSetting(t, pool, "lock_timeout"))
}

// sessionSetting reads a timeout the server actually holds, in milliseconds
// — the value the engine's bounds really have, not the one it asked for.
func sessionSetting(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var ms int64
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = $1", name).Scan(&ms))
	return ms
}
