package migrate

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// The type change is committed before its statistics are refreshed, so a
// refresh that cannot run does not fail the run: the verdict's detail names
// the ANALYZE the operator still has to run, with every identifier quoted,
// and that command runs as written once the lock is free. Another session's
// VACUUM or ANALYZE holds the same lock the refresh needs; here a
// transaction holding SHARE UPDATE EXCLUSIVE stands in for it.
func TestRefreshRetypedStatisticsNamesTheAnalyzeItCouldNotRun(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cases := []struct {
		name   string
		table  string
		column string
		// want is the recovery command, with %s standing for the schema.
		want string
	}{
		{"plain names", "t", "code", `run ANALYZE "%s"."t" ("code")`},
		{"names that need quoting", "Order Items", "Status Code", `run ANALYZE "%s"."Order Items" ("Status Code")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			table := pgx.Identifier{schema, tc.table}.Sanitize()
			column := pgx.Identifier{tc.column}.Sanitize()
			_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, %s varchar(100))", table, column))
			require.NoError(t, err)
			pt, err := preflight.CheckTable(t.Context(), pool, schema, tc.table, preflight.NoSizeLimit)
			require.NoError(t, err)
			st, err := statement.ParseOne(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE varchar(100)", table, column))
			require.NoError(t, err)

			holder, err := pool.Begin(t.Context())
			require.NoError(t, err)
			defer func() {
				// Redundant closer: the holder is rolled back below on the
				// success path.
				_ = holder.Rollback(t.Context())
			}()
			_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s IN SHARE UPDATE EXCLUSIVE MODE", table))
			require.NoError(t, err)

			budget := executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second}
			detail := refreshRetypedStatistics(t.Context(), pool, pt, st, budget, slog.New(slog.DiscardHandler))
			require.NoError(t, holder.Rollback(t.Context()))

			assert.Contains(t, detail, fmt.Sprintf("the statistics of %s were not refreshed after the type change", tc.column))
			want := fmt.Sprintf(tc.want, schema)
			require.True(t, strings.HasSuffix(detail, want), "detail %q ends with %q", detail, want)
			_, err = pool.Exec(t.Context(), strings.TrimPrefix(want, "run "))
			require.NoError(t, err, "the recovery command runs as written")
		})
	}
}
