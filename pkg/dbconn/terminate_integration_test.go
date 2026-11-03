package dbconn_test

import (
	"context"
	"fmt"
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
// of pg_catalog resolves an unqualified function to a same-signature
// lookalike planted there. The statement qualifies every function it calls,
// so each test below plants one lookalike and proves it is never consulted:
// the holder is the pid reported, and the holder really exits.

// A pg_terminate_backend that returns true and signals nothing would make
// the call report the blocker evicted while it keeps the lock.
func TestTerminateBlockersIgnoresAShadowingTerminateBackend(t *testing.T) {
	url, pool, schema := newShadowingFixture(t)
	testutil.ExecInSchema(t, pool, schema, `
		CREATE FUNCTION pg_terminate_backend(integer) RETURNS boolean
			LANGUAGE sql AS 'SELECT true'`)

	assertTerminatesOnlyTheHolder(t, url, pool, schema)
}

// A pg_blocking_pids that names a bystander would make the call terminate
// the bystander and report it as the blocker, while the holder keeps the lock.
func TestTerminateBlockersIgnoresAShadowingBlockingPids(t *testing.T) {
	url, pool, schema := newShadowingFixture(t)
	bystander, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(bystander.Release)
	var bystanderPid int
	require.NoError(t, bystander.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&bystanderPid))
	testutil.ExecInSchema(t, pool, schema, fmt.Sprintf(`
		CREATE FUNCTION pg_blocking_pids(integer) RETURNS integer[]
			LANGUAGE sql AS 'SELECT ARRAY[%d]'`, bystanderPid))

	assertTerminatesOnlyTheHolder(t, url, pool, schema)
	alive, err := backendAlive(t.Context(), pool, bystanderPid)
	require.NoError(t, err)
	assert.True(t, alive, "the bystander must not be terminated")
}

// An unnest that returns no rows would make the call report nothing
// terminated, so the cutover's eviction would silently do nothing.
func TestTerminateBlockersIgnoresAShadowingUnnest(t *testing.T) {
	url, pool, schema := newShadowingFixture(t)
	testutil.ExecInSchema(t, pool, schema, `
		CREATE FUNCTION unnest(anyarray) RETURNS SETOF anyelement
			LANGUAGE sql AS 'SELECT $1[1] WHERE false'`)

	assertTerminatesOnlyTheHolder(t, url, pool, schema)
}

// ServerVersion and ServerMajor also run on the caller's pool. A
// current_setting that reports another server would make a version-gated
// proof apply the wrong server's rules.
func TestServerVersionIgnoresAShadowingCurrentSetting(t *testing.T) {
	url, pool, schema := newShadowingFixture(t)
	wantVersion, err := dbconn.ServerVersion(t.Context(), pool)
	require.NoError(t, err)
	wantMajor, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)
	testutil.ExecInSchema(t, pool, schema, `
		CREATE FUNCTION current_setting(text) RETURNS text
			LANGUAGE sql AS 'SELECT ''990000'''`)
	caller := testutil.NewCatalogShadowingPool(t, url, schema)

	gotVersion, err := dbconn.ServerVersion(t.Context(), caller)
	require.NoError(t, err)
	gotMajor, err := dbconn.ServerMajor(t.Context(), caller)
	require.NoError(t, err)
	assert.Equal(t, wantVersion, gotVersion)
	assert.Equal(t, wantMajor, gotMajor)
}

// newShadowingFixture starts a server and returns its URL, a NewPool pool,
// and a fresh schema holding the swap_target table the lookalikes sit beside.
func newShadowingFixture(t *testing.T) (string, *pgxpool.Pool, string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	testutil.ExecInSchema(t, pool, schema, `
		CREATE TABLE swap_target (
			id int PRIMARY KEY
		)`)
	return url, pool, schema
}

// assertTerminatesOnlyTheHolder queues one backend behind another on
// swap_target, calls TerminateBlockers for the waiter on a pool whose path
// puts schema ahead of pg_catalog, and asserts the holder is the one pid
// reported and that it exits.
func assertTerminatesOnlyTheHolder(t *testing.T, url string, pool *pgxpool.Pool, schema string) {
	t.Helper()
	caller := testutil.NewCatalogShadowingPool(t, url, schema)
	holderPid, waiterPid := blockOneBackendBehindAnother(t, pool, schema+".swap_target")

	terminated, err := dbconn.TerminateBlockers(t.Context(), caller, waiterPid)
	require.NoError(t, err)
	assert.Equal(t, []int{holderPid}, terminated, "only the holder should be reported terminated")

	const exitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		alive, err := backendAlive(t.Context(), pool, holderPid)
		return err == nil && !alive
	}, exitDeadline, 50*time.Millisecond,
		"the blocker the call reported as terminated is still running")
}

// backendAlive reports whether pid is still a server backend.
func backendAlive(ctx context.Context, pool *pgxpool.Pool, pid int) (bool, error) {
	var alive bool
	err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid = $1)", pid).Scan(&alive)
	return alive, err
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
