package dbconn_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// A database whose search_path names pg_catalog after a user schema lets a
// table in that schema shadow the catalog: `decoy.pg_class` would answer an
// unqualified `pg_class` read. Every pooled session drops the shadowed entry
// so the catalog is searched implicitly first again, while a path pg_catalog
// leads is left alone and every other entry — and therefore the creation
// schema — stays exactly as configured. The decoy schema also shadows
// set_config itself with a function that changes nothing, so the rewrite
// only lands if the hook qualifies the call it makes under the shadowed
// path. The setting is database-scoped on a throwaway database, so no other
// connection on the server sees it.
func TestPoolRemovesShadowedCatalogFromSearchPath(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	setup, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(setup.Close)
	_, err = setup.Exec(t.Context(), `CREATE SCHEMA decoy;
		CREATE TABLE decoy.pg_class (oid oid, relname name, relnamespace oid);
		INSERT INTO decoy.pg_class VALUES (1, 'bogus', 1);
		CREATE FUNCTION decoy.set_config(text, text, boolean) RETURNS text
			LANGUAGE sql AS $$ SELECT 'decoy'::text $$`)
	require.NoError(t, err)
	var database string
	require.NoError(t, setup.QueryRow(t.Context(), "SELECT current_database()").Scan(&database))
	databaseName := pgx.Identifier{database}.Sanitize()

	testCases := []struct {
		name       string
		searchPath string
		wantPath   string
		// wantCreationSchema is what current_schema() reports; empty means
		// NULL, the state in which an unqualified CREATE has nowhere to land.
		wantCreationSchema string
	}{
		{name: "catalog after user schema", searchPath: "decoy, pg_catalog", wantPath: "decoy", wantCreationSchema: "decoy"},
		{name: "catalog before user schema", searchPath: "pg_catalog, decoy", wantPath: "pg_catalog, decoy", wantCreationSchema: "pg_catalog"},
		{name: "catalog only", searchPath: "pg_catalog", wantPath: "pg_catalog", wantCreationSchema: "pg_catalog"},
		{name: "quoted catalog", searchPath: `decoy, "pg_catalog"`, wantPath: "decoy", wantCreationSchema: "decoy"},
		{name: "default path untouched", searchPath: `"$user", public`, wantPath: `"$user", public`, wantCreationSchema: "public"},
		{name: "only a missing schema ahead leaves no creation schema", searchPath: "no_such_schema, pg_catalog", wantPath: "no_such_schema", wantCreationSchema: ""},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := setup.Exec(t.Context(), "ALTER DATABASE "+databaseName+" SET search_path TO "+tc.searchPath)
			require.NoError(t, err)
			pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
			require.NoError(t, err)
			t.Cleanup(pool.Close)

			var gotPath string
			require.NoError(t, pool.QueryRow(t.Context(), "SHOW search_path").Scan(&gotPath))
			assert.Equal(t, tc.wantPath, gotPath)
			var creationSchema *string
			require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&creationSchema))
			if tc.wantCreationSchema == "" {
				assert.Nil(t, creationSchema, "current_schema() must be NULL")
			} else {
				require.NotNil(t, creationSchema)
				assert.Equal(t, tc.wantCreationSchema, *creationSchema)
			}

			// The decoy holds one row; the real catalog holds far more.
			var catalogRows int
			require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_class").Scan(&catalogRows))
			assert.Greater(t, catalogRows, 1)
		})
	}
}

func TestPoolIntegration(t *testing.T) {
	url := testutil.StartPostgres(t)

	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:              url,
		LockTimeout:      300 * time.Millisecond,
		StatementTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("session timeouts are applied", func(t *testing.T) {
		var lockTimeout, stmtTimeout string
		require.NoError(t, pool.QueryRow(t.Context(), "SHOW lock_timeout").Scan(&lockTimeout))
		require.NoError(t, pool.QueryRow(t.Context(), "SHOW statement_timeout").Scan(&stmtTimeout))
		assert.Equal(t, "300ms", lockTimeout)
		assert.Equal(t, "500ms", stmtTimeout)
	})

	t.Run("server version reads server_version", func(t *testing.T) {
		version, err := dbconn.ServerVersion(t.Context(), pool)
		require.NoError(t, err)
		var want string
		require.NoError(t, pool.QueryRow(t.Context(),
			"SELECT current_setting('server_version')").Scan(&want))
		assert.Equal(t, want, version)
	})

	t.Run("absent concurrent index progress is not an error", func(t *testing.T) {
		observation, active, err := dbconn.ConcurrentIndexProgress(t.Context(), pool, 0)
		require.NoError(t, err)
		assert.False(t, active)
		assert.Empty(t, observation.Phase)
	})

	t.Run("statement_timeout cancels runaway work", func(t *testing.T) {
		_, err := pool.Exec(t.Context(), "SELECT pg_sleep(5)")
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "57014", pgErr.Code, "expected query_canceled from statement_timeout")
	})

	t.Run("lock wait beyond lock_timeout is a retryable error", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		table := schema + ".locked"
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+table+" (id int primary key)")
		require.NoError(t, err)

		holder, err := pool.Acquire(t.Context())
		require.NoError(t, err)
		defer holder.Release()
		tx, err := holder.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(t.Context()) }()
		_, err = tx.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE")
		require.NoError(t, err)

		_, err = pool.Exec(t.Context(), "INSERT INTO "+table+" VALUES (1)")
		require.Error(t, err)
		assert.True(t, dbconn.Retryable(err), "lock_not_available must classify as retryable, got: %v", err)
	})

	t.Run("TerminateBlockers evicts exactly the blocking backend", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		table := schema + ".swap_target"
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+table+" (id int primary key)")
		require.NoError(t, err)

		// Backend A holds ACCESS EXCLUSIVE in an open transaction.
		connA, err := pool.Acquire(t.Context())
		require.NoError(t, err)
		defer connA.Release()
		var aPid int
		require.NoError(t, connA.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&aPid))
		txA, err := connA.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = txA.Rollback(t.Context()) }()
		_, err = txA.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE")
		require.NoError(t, err)

		// Backend B queues behind A with a generous lock_timeout.
		connB, err := pool.Acquire(t.Context())
		require.NoError(t, err)
		defer connB.Release()
		var bPid int
		require.NoError(t, connB.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&bPid))
		_, err = connB.Exec(t.Context(), "SET lock_timeout = '30s'")
		require.NoError(t, err)
		insertDone := make(chan error, 1)
		go func() {
			_, err := connB.Exec(t.Context(), "INSERT INTO "+table+" VALUES (1)")
			insertDone <- err
		}()

		const blockedDeadline = 10 * time.Second
		require.Eventually(t, func() bool {
			var blocked bool
			err := pool.QueryRow(t.Context(),
				"SELECT cardinality(pg_blocking_pids($1::int)) > 0", bPid).Scan(&blocked)
			return err == nil && blocked
		}, blockedDeadline, 50*time.Millisecond, "backend B never showed up as blocked behind A")

		terminated, err := dbconn.TerminateBlockers(t.Context(), pool, bPid)
		require.NoError(t, err)
		assert.Equal(t, []int{aPid}, terminated, "only the holder should be terminated")

		const insertDeadline = 10 * time.Second
		select {
		case err := <-insertDone:
			require.NoError(t, err, "B's insert should succeed once the blocker is evicted")
		case <-time.After(insertDeadline):
			t.Fatalf("B's insert still blocked %s after terminating the blocker", insertDeadline)
		}
	})
}
