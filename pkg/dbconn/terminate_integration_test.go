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

// TerminateBlockers runs on whatever pool the caller hands it, under that
// session's own search_path. A caller's pool whose path lists a schema ahead
// of pg_catalog resolves an unqualified pg_terminate_backend to a lookalike
// planted there; one that returns true and signals nothing would make the
// call report the blocker evicted while it keeps the lock. The statement
// qualifies every function it calls, so the lookalike is never consulted
// and the blocker really exits.
func TestTerminateBlockersIgnoresTheCallersShadowingSearchPath(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema := testutil.NewSchema(t, pool)
	testutil.ExecInSchema(t, pool, schema, `
		CREATE TABLE swap_target (id int primary key);
		CREATE FUNCTION pg_terminate_backend(integer) RETURNS boolean
			LANGUAGE sql AS 'SELECT true'`)
	table := schema + ".swap_target"
	caller := testutil.NewCatalogShadowingPool(t, url, schema)

	aPid, bPid := blockOneBackendBehindAnother(t, pool, table)

	terminated, err := dbconn.TerminateBlockers(t.Context(), caller, bPid)
	require.NoError(t, err)
	assert.Equal(t, []int{aPid}, terminated, "only the holder should be reported terminated")

	const exitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var alive bool
		err := pool.QueryRow(t.Context(),
			"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid = $1)", aPid).Scan(&alive)
		return err == nil && !alive
	}, exitDeadline, 50*time.Millisecond,
		"the blocker the call reported as terminated is still running")
}

// blockOneBackendBehindAnother holds ACCESS EXCLUSIVE on table in one
// backend's open transaction, queues a second backend's insert behind it,
// waits until pg_blocking_pids shows the queue, and returns both pids: the
// holder first, the waiter second. Both backends are released at cleanup.
func blockOneBackendBehindAnother(t *testing.T, pool *pgxpool.Pool, table string) (holderPid, waiterPid int) {
	t.Helper()

	connA, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(connA.Release)
	require.NoError(t, connA.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&holderPid))
	txA, err := connA.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = txA.Rollback(t.Context()) })
	_, err = txA.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)

	connB, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(connB.Release)
	require.NoError(t, connB.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&waiterPid))
	_, err = connB.Exec(t.Context(), "SET lock_timeout = '30s'")
	require.NoError(t, err)
	insertDone := make(chan error, 1)
	go func() {
		_, err := connB.Exec(t.Context(), "INSERT INTO "+table+" VALUES (1)")
		insertDone <- err
	}()
	t.Cleanup(func() {
		const insertSettleDeadline = 30 * time.Second
		select {
		case <-insertDone:
		case <-time.After(insertSettleDeadline):
			t.Logf("the waiter's insert did not settle within %s", insertSettleDeadline)
		}
	})

	const blockedDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var blocked bool
		err := pool.QueryRow(t.Context(),
			"SELECT cardinality(pg_blocking_pids($1::int)) > 0", waiterPid).Scan(&blocked)
		return err == nil && blocked
	}, blockedDeadline, 50*time.Millisecond, "the waiter never showed up as blocked behind the holder")
	return holderPid, waiterPid
}
