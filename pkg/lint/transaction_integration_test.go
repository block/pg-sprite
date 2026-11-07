package lint_test

import (
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
