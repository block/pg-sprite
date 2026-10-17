package lint_test

import (
	"errors"
	"fmt"
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

func hasCode(report lint.Report, stmt int, code lint.Code) bool {
	for _, f := range report.Findings {
		if f.Statement == stmt && f.Code == code {
			return true
		}
	}
	return false
}
