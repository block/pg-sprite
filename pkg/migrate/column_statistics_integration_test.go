package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// ALTER COLUMN TYPE deletes the column's planner statistics even when it is
// a catalog relabel, and on a table that changes slowly autovacuum may not
// analyze it again for a long time. A committed type change therefore
// leaves the retyped columns analyzed, and the verdict says so; a column
// the statement did not retype keeps the statistics it had.
func TestRunRefreshesTheStatisticsOfRetypedColumns(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id int PRIMARY KEY, code varchar(50), label varchar(50), note text)",
		"INSERT INTO %[1]s.t SELECT g, 'c' || (g %% 10), 'l' || (g %% 4), 'n' FROM generate_series(1, 2000) g",
		"ANALYZE %[1]s.t",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	require.True(t, hasColumnStatistics(t, pool, schema, "note"))

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %s.t ALTER COLUMN code TYPE varchar(100), ALTER COLUMN label TYPE text", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Contains(t, v.Detail, "the statistics of code, label were refreshed after the type change")

	assert.True(t, hasColumnStatistics(t, pool, schema, "code"), "the retyped column is analyzed again")
	assert.True(t, hasColumnStatistics(t, pool, schema, "label"), "the retyped column is analyzed again")
	assert.True(t, hasColumnStatistics(t, pool, schema, "note"), "a column the statement did not retype keeps its statistics")
}

// A change that retypes no column runs no ANALYZE and says nothing about
// statistics.
func TestRunLeavesStatisticsAloneWithoutATypeChange(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, code varchar(50))", schema))
	require.NoError(t, err)

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %s.t ADD COLUMN created timestamptz", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.NotContains(t, v.Detail, "statistics")
	assert.False(t, hasColumnStatistics(t, pool, schema, "code"), "nothing analyzed the table")
}

// hasColumnStatistics reports whether the planner has statistics for the
// column.
func hasColumnStatistics(t *testing.T, pool *pgxpool.Pool, schema, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM pg_stats WHERE schemaname = $1 AND tablename = 't' AND attname = $2)",
		schema, column).Scan(&exists))
	return exists
}
