package lint_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/lint"
)

// The concurrent-in-transaction finding rests on PostgreSQL refusing every
// CONCURRENTLY form inside an explicit block. Each case runs a script lint
// flags, statement by statement on one session, and requires the server
// to refuse the flagged statement with active_sql_transaction.
func TestServerRefusesWhatConcurrentInTransactionFlags(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	for _, sql := range []string{
		"CREATE INDEX CONCURRENTLY t_c_new ON %[1]s.t (c)",
		"DROP INDEX CONCURRENTLY %[1]s.t_c_idx",
		"REINDEX INDEX CONCURRENTLY %[1]s.t_c_idx",
		"REINDEX (CONCURRENTLY true) INDEX %[1]s.t_c_idx",
		"ALTER TABLE %[1]s.p DETACH PARTITION %[1]s.p_low CONCURRENTLY",
	} {
		t.Run(sql, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			for _, ddl := range []string{
				"CREATE TABLE %[1]s.t (c int)",
				"CREATE INDEX t_c_idx ON %[1]s.t (c)",
				"CREATE TABLE %[1]s.p (id int) PARTITION BY RANGE (id)",
				"CREATE TABLE %[1]s.p_low PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (10)",
			} {
				_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
				require.NoError(t, err)
			}
			flagged := fmt.Sprintf(sql, schema)
			report, err := lint.Check("BEGIN; " + flagged + "; COMMIT")
			require.NoError(t, err)
			require.True(t, hasCode(report, 2, lint.CodeConcurrentInTransaction), "lint flags %s inside a block", flagged)

			conn, err := pool.Acquire(t.Context())
			require.NoError(t, err)
			defer conn.Release()
			_, err = conn.Exec(t.Context(), "BEGIN")
			require.NoError(t, err)
			_, err = conn.Exec(t.Context(), flagged)
			var pgErr *pgconn.PgError
			require.True(t, errors.As(err, &pgErr), "the server refuses %s inside a block: %v", flagged, err)
			assert.Equal(t, "25001", pgErr.Code, "active_sql_transaction for %s", flagged)
			_, err = conn.Exec(t.Context(), "ROLLBACK")
			require.NoError(t, err)
		})
	}
}

// A REINDEX whose CONCURRENTLY option is switched off rebuilds with the
// blocking lock, which PostgreSQL runs inside a block. Lint leaves it
// unflagged, and the server runs it.
func TestServerRunsAReindexWithConcurrentlyOff(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	for _, sql := range []string{
		"REINDEX (CONCURRENTLY false) INDEX %[1]s.t_c_idx",
		"REINDEX (CONCURRENTLY 0) INDEX %[1]s.t_c_idx",
		"REINDEX (CONCURRENTLY off) INDEX %[1]s.t_c_idx",
	} {
		t.Run(sql, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			for _, ddl := range []string{"CREATE TABLE %[1]s.t (c int)", "CREATE INDEX t_c_idx ON %[1]s.t (c)"} {
				_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
				require.NoError(t, err)
			}
			stmt := fmt.Sprintf(sql, schema)
			report, err := lint.Check("BEGIN; " + stmt + "; COMMIT")
			require.NoError(t, err)
			assert.False(t, hasCode(report, 2, lint.CodeConcurrentInTransaction), "lint leaves %s unflagged", stmt)

			conn, err := pool.Acquire(t.Context())
			require.NoError(t, err)
			defer conn.Release()
			_, err = conn.Exec(t.Context(), "BEGIN")
			require.NoError(t, err)
			_, err = conn.Exec(t.Context(), stmt)
			require.NoError(t, err, "the server runs %s inside a block", stmt)
			_, err = conn.Exec(t.Context(), "COMMIT")
			require.NoError(t, err)
		})
	}
}

func hasCode(report lint.Report, stmt int, code lint.Code) bool {
	for _, f := range report.Findings {
		if f.Statement == stmt && f.Code == code {
			return true
		}
	}
	return false
}

// The validate-in-transaction finding, and the planner's refusal of a
// VALIDATE beside another operation in one ALTER TABLE, rest on PostgreSQL
// holding every lock until the transaction ends. Each case runs a script
// lint flags on one session and reads the locks the session holds on the
// table once the VALIDATE has run: still the earlier statement's, which
// blocks writes or reads. The same VALIDATE run in a transaction of its
// own holds only SHARE UPDATE EXCLUSIVE.
func TestServerHoldsTheEarlierLockWhileAValidateInTheSameTransactionScans(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// locksAfter runs stmts in one transaction and returns the lock modes
	// the session then holds on the table.
	locksAfter := func(t *testing.T, schema string, stmts ...string) []string {
		t.Helper()
		conn, err := pool.Acquire(t.Context())
		require.NoError(t, err)
		defer conn.Release()
		tx, err := conn.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		for _, stmt := range stmts {
			_, err := tx.Exec(t.Context(), fmt.Sprintf(stmt, schema))
			require.NoError(t, err, stmt)
		}
		var modes []string
		require.NoError(t, tx.QueryRow(t.Context(), `SELECT pg_catalog.array_agg(mode ORDER BY mode)
			FROM pg_catalog.pg_locks
			WHERE pid OPERATOR(pg_catalog.=) pg_catalog.pg_backend_pid()
			  AND relation OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)`, schema+".t").Scan(&modes))
		return modes
	}
	setup := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		for _, ddl := range []string{
			"CREATE TABLE %[1]s.p (id int PRIMARY KEY)",
			"CREATE TABLE %[1]s.t (n int, pid int)",
			"INSERT INTO %[1]s.p VALUES (1)",
			"INSERT INTO %[1]s.t VALUES (1, 1)",
			"ALTER TABLE %[1]s.t ADD CONSTRAINT t_old CHECK (n > -1) NOT VALID",
		} {
			_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
			require.NoError(t, err)
		}
		return schema
	}

	for _, tc := range []struct {
		name  string
		stmts []string
		held  string
	}{
		{"check then validate", []string{
			"ALTER TABLE %[1]s.t ADD CONSTRAINT t_n_pos CHECK (n > 0) NOT VALID",
			"ALTER TABLE %[1]s.t VALIDATE CONSTRAINT t_n_pos",
		}, "AccessExclusiveLock"},
		{"foreign key then validate", []string{
			"ALTER TABLE %[1]s.t ADD CONSTRAINT t_pid_fk FOREIGN KEY (pid) REFERENCES %[1]s.p (id) NOT VALID",
			"ALTER TABLE %[1]s.t VALIDATE CONSTRAINT t_pid_fk",
		}, "ShareRowExclusiveLock"},
		{"added column then validate", []string{
			"ALTER TABLE %[1]s.t ADD COLUMN x int",
			"ALTER TABLE %[1]s.t VALIDATE CONSTRAINT t_old",
		}, "AccessExclusiveLock"},
		{"one statement", []string{
			"ALTER TABLE %[1]s.t ADD CONSTRAINT t_n_pos CHECK (n > 0) NOT VALID, VALIDATE CONSTRAINT t_n_pos",
		}, "AccessExclusiveLock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := setup(t)
			script := "BEGIN; " + fmt.Sprintf(strings.Join(tc.stmts, "; "), schema) + "; COMMIT"
			report, err := lint.Check(script)
			require.NoError(t, err)
			last := len(tc.stmts) + 1
			flagged := hasCode(report, last, lint.CodeValidateInTransaction) || hasCode(report, last, lint.CodeBlockingIdiom)
			require.True(t, flagged, "lint flags the VALIDATE in %s", script)
			assert.Contains(t, locksAfter(t, schema, tc.stmts...), tc.held,
				"the earlier lock is still held once the VALIDATE has run")
		})
	}

	t.Run("in a transaction of its own", func(t *testing.T) {
		schema := setup(t)
		assert.Equal(t, []string{"ShareUpdateExclusiveLock"},
			locksAfter(t, schema, "ALTER TABLE %[1]s.t VALIDATE CONSTRAINT t_old"))
	})
}

// Lint flags a VALIDATE inside a block exactly when the server, at that
// point, holds a lock on the table that blocks reads or writes. Each case
// runs the statements before the VALIDATE on one session and reads the
// locks it holds: a nested BEGIN keeps them, ROLLBACK TO SAVEPOINT
// releases the ones taken since the savepoint while RELEASE keeps them,
// REINDEX TABLE holds SHARE, TRUNCATE holds ACCESS EXCLUSIVE, LOCK TABLE
// holds the mode it names, a renamed table keeps its lock under the new
// name, and SET STATISTICS, a column option, and most storage parameters
// hold only SHARE UPDATE EXCLUSIVE.
func TestLintFlagsAValidateExactlyWhenTheServerHoldsABlockingLock(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	blocking := map[string]bool{
		"ShareLock": true, "ShareRowExclusiveLock": true, "ExclusiveLock": true, "AccessExclusiveLock": true,
	}
	for _, tc := range []struct {
		name  string
		stmts []string
		held  bool
		table string
	}{
		{"nested begin", []string{"ALTER TABLE %[1]s.t ADD COLUMN x int", "BEGIN"}, true, ""},
		{"lock table", []string{"LOCK TABLE %[1]s.t"}, true, ""},
		{"lock table in share mode", []string{"LOCK TABLE %[1]s.t IN SHARE MODE"}, true, ""},
		{"lock table in row exclusive mode", []string{"LOCK TABLE %[1]s.t IN ROW EXCLUSIVE MODE"}, false, ""},
		{"truncate", []string{"TRUNCATE %[1]s.t"}, true, ""},
		{"renamed table", []string{"ALTER TABLE %[1]s.t RENAME TO u"}, true, "u"},
		{"reindex table", []string{"REINDEX TABLE %[1]s.t"}, true, ""},
		{"set storage", []string{"ALTER TABLE %[1]s.t ALTER COLUMN n SET STORAGE PLAIN"}, true, ""},
		{"user_catalog_table", []string{"ALTER TABLE %[1]s.t SET (user_catalog_table = true)"}, true, ""},
		{"set statistics", []string{"ALTER TABLE %[1]s.t ALTER COLUMN n SET STATISTICS 500"}, false, ""},
		{"column option", []string{"ALTER TABLE %[1]s.t ALTER COLUMN n SET (n_distinct = 100)"}, false, ""},
		{"fillfactor", []string{"ALTER TABLE %[1]s.t SET (fillfactor = 70)"}, false, ""},
		{"autovacuum reset", []string{"ALTER TABLE %[1]s.t RESET (autovacuum_enabled, toast.autovacuum_enabled)"}, false, ""},
		{"rolled back to a savepoint", []string{"SAVEPOINT s", "ALTER TABLE %[1]s.t ADD COLUMN x int", "ROLLBACK TO s"}, false, ""},
		{"locked before the savepoint", []string{
			"ALTER TABLE %[1]s.t ADD COLUMN y int", "SAVEPOINT s", "ALTER TABLE %[1]s.t ADD COLUMN x int", "ROLLBACK TO s",
		}, true, ""},
		{"released savepoint", []string{"SAVEPOINT s", "ALTER TABLE %[1]s.t ADD COLUMN x int", "RELEASE s"}, true, ""},
		{"rolled back to an outer savepoint", []string{
			"SAVEPOINT a", "SAVEPOINT b", "ALTER TABLE %[1]s.t ADD COLUMN x int", "ROLLBACK TO a",
		}, false, ""},
		{"rolled back to an inner savepoint", []string{
			"SAVEPOINT a", "ALTER TABLE %[1]s.t ADD COLUMN y int", "SAVEPOINT b", "ALTER TABLE %[1]s.t ADD COLUMN x int", "ROLLBACK TO b",
		}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			for _, ddl := range []string{
				"CREATE TABLE %[1]s.t (n int)",
				"CREATE INDEX t_n_idx ON %[1]s.t (n)",
				"ALTER TABLE %[1]s.t ADD CONSTRAINT t_n_pos CHECK (n > 0) NOT VALID",
			} {
				_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
				require.NoError(t, err)
			}
			stmts := make([]string, len(tc.stmts))
			for i, stmt := range tc.stmts {
				stmts[i] = strings.ReplaceAll(stmt, "%[1]s", schema)
			}
			table := cmp.Or(tc.table, "t")
			validate := fmt.Sprintf("ALTER TABLE %s.%s VALIDATE CONSTRAINT t_n_pos", schema, table)
			report, err := lint.Check("BEGIN; " + strings.Join(stmts, "; ") + "; " + validate + "; COMMIT")
			require.NoError(t, err)
			assert.Equal(t, tc.held, hasCode(report, len(stmts)+2, lint.CodeValidateInTransaction),
				"lint flags the VALIDATE exactly when a blocking lock is held")

			conn, err := pool.Acquire(t.Context())
			require.NoError(t, err)
			defer conn.Release()
			defer func() { _, _ = conn.Exec(context.WithoutCancel(t.Context()), "ROLLBACK") }()
			for _, stmt := range append([]string{"BEGIN"}, stmts...) {
				_, err := conn.Exec(t.Context(), stmt)
				require.NoError(t, err, stmt)
			}
			var modes []string
			require.NoError(t, conn.QueryRow(t.Context(), `SELECT pg_catalog.array_agg(mode)
				FROM pg_catalog.pg_locks
				WHERE pid OPERATOR(pg_catalog.=) pg_catalog.pg_backend_pid()
				  AND relation OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)`, schema+"."+table).Scan(&modes))
			held := false
			for _, mode := range modes {
				held = held || blocking[mode]
			}
			assert.Equal(t, tc.held, held, "the server holds %v before the VALIDATE", modes)
		})
	}
}
