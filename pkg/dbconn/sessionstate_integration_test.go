package dbconn_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// A statement-mode pooler returns the backend to its pool after every
// statement and refuses any transaction that spans more than one, so the
// engine has neither a session for its bounds and locks nor a transaction for
// a chunk copy, a guarded write, or the cutover. The affinity proof's own
// transaction is the first thing to hit the refusal, and the pool reports it
// as the typed no-session-affinity refusal, naming what to change, so a
// caller routes it like the transaction-mode case above.
func TestNewPoolRefusesStatementPooling(t *testing.T) {
	pooledURL := testutil.StartPostgresBehindPgBouncer(t, testutil.StatementPooling)

	_, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: pooledURL})

	require.ErrorIs(t, err, dbconn.ErrNoSessionAffinity)
	assert.Contains(t, err.Error(), "session-mode endpoint",
		"the refusal must name what to change")
	// The pooler's own refusal stays in the chain, so the detail names it.
	// PgBouncer reports it as protocol_violation and closes the connection.
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "08P01", pgErr.Code)
	assert.Equal(t, "FATAL", pgErr.Severity)
}

// A session-mode pooler with no backend to spare makes a client wait, and
// PgBouncer ends the wait with the same FATAL protocol_violation it uses to
// refuse a transaction block in statement mode. The pool here has two
// backends: another client holds one, the connection under proof takes the
// other, and the proof's second connection waits for a third until
// query_wait_timeout fires. The pinner is a plain pool, so nothing has run
// on its connection before the proof does. That is a supported endpoint
// short of capacity, not one without session affinity, so the proof fails
// with the pooler's error and not with the typed refusal that would tell the
// operator to change the endpoint.
func TestProveSessionAffinityDoesNotReadAnExhaustedSessionPoolAsNoAffinity(t *testing.T) {
	pooledURL := testutil.StartPostgresBehindPgBouncerWith(t, testutil.SessionPooling, map[string]string{
		"DEFAULT_POOL_SIZE":  "2",
		"QUERY_WAIT_TIMEOUT": "2",
	})
	// Session mode keeps the backend a client's first statement got until
	// the client disconnects, so each of these holds one of the two.
	connectAndPinBackend(t, pooledURL)
	conn := connectAndPinBackend(t, pooledURL)
	pinnerCfg, err := pgxpool.ParseConfig(pooledURL)
	require.NoError(t, err)
	pinnerCfg.MaxConns = 1
	pinner, err := pgxpool.NewWithConfig(t.Context(), pinnerCfg)
	require.NoError(t, err)
	t.Cleanup(pinner.Close)

	err = dbconn.ProveSessionAffinity(t.Context(), conn, pinner, 0)

	require.Error(t, err)
	assert.NotErrorIs(t, err, dbconn.ErrNoSessionAffinity)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the pooler's own error stays in the chain")
	assert.Equal(t, "08P01", pgErr.Code)
}

// connectAndPinBackend opens a client connection to url and runs one
// statement on it, so a session-mode pooler binds it a backend.
func connectAndPinBackend(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close(context.WithoutCancel(t.Context()))) })
	_, err = conn.Exec(t.Context(), "SELECT 1")
	require.NoError(t, err)
	return conn
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

// A timeout below one millisecond is refused before any connection is made,
// by both ways of opening a session (LK-2). PostgreSQL counts these settings
// in whole milliseconds and reads 0 as no timeout, so such a request would
// reach the server as no bound at all, and a read-back comparing 0 with 0
// could not tell. One millisecond, the smallest bound the server can hold,
// opens a session that carries it.
func TestSubMillisecondTimeoutNeverYieldsAnUnboundedSession(t *testing.T) {
	url := testutil.StartPostgres(t)

	_, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url, LockTimeout: 500 * time.Microsecond})
	require.ErrorIs(t, err, dbconn.ErrInvalidTimeout)

	conn, err := dbconn.ConnectSession(t.Context(), dbconn.Config{URL: url, StatementTimeout: 500 * time.Microsecond})
	require.ErrorIs(t, err, dbconn.ErrInvalidTimeout)
	assert.Nil(t, conn)

	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url, LockTimeout: time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	assert.Equal(t, int64(1), sessionSetting(t, pool, "lock_timeout"))
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
