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
	t.Cleanup(pool.Close)
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
	require.NotNil(t, v.StatisticsRefresh)
	assert.Equal(t, verdict.StatisticsRefresh{Columns: []string{"code", "label"}, Refreshed: true}, *v.StatisticsRefresh)

	assert.True(t, hasColumnStatistics(t, pool, schema, "code"), "the retyped column is analyzed again")
	assert.True(t, hasColumnStatistics(t, pool, schema, "label"), "the retyped column is analyzed again")
	assert.True(t, hasColumnStatistics(t, pool, schema, "note"), "a column the statement did not retype keeps its statistics")
}

// A change that retypes no column runs no ANALYZE and says nothing about
// statistics.
func TestRunLeavesStatisticsAloneWithoutATypeChange(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, code varchar(50))", schema))
	require.NoError(t, err)

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %s.t ADD COLUMN created timestamptz", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Nil(t, v.StatisticsRefresh)
	assert.NotContains(t, v.Detail, "statistics")
	assert.False(t, hasColumnStatistics(t, pool, schema, "code"), "nothing analyzed the table")
}

// A type change rebuilds an expression index over the column and recreates
// an extended statistics object that covers it, deleting what the planner
// knew about both. The server rebuilds those statistics only on a
// whole-table ANALYZE, which is the one the refresh runs, so both are
// back after the run.
func TestRunRefreshesExpressionIndexAndExtendedStatistics(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id int PRIMARY KEY, code varchar(50), region varchar(50))",
		"CREATE INDEX t_lower_code ON %[1]s.t (lower(code))",
		"CREATE STATISTICS %[1]s.t_code_region (dependencies) ON code, region FROM %[1]s.t",
		"INSERT INTO %[1]s.t SELECT g, 'C' || (g %% 10), 'r' || (g %% 10) FROM generate_series(1, 2000) g",
		"ANALYZE %[1]s.t",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	require.True(t, hasExpressionIndexStatistics(t, pool, schema, "t_lower_code"))
	require.True(t, hasDependencyStatistics(t, pool, schema, "t_code_region"))

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %s.t ALTER COLUMN code TYPE varchar(100)", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	require.NotNil(t, v.StatisticsRefresh)
	assert.True(t, v.StatisticsRefresh.Refreshed)

	assert.True(t, hasExpressionIndexStatistics(t, pool, schema, "t_lower_code"), "the rebuilt expression index is analyzed again")
	assert.True(t, hasDependencyStatistics(t, pool, schema, "t_code_region"), "the recreated extended statistics are built again")
}

// A type change on an inheritance parent recurses into its children and
// deletes their statistics too, and ANALYZE on the parent leaves them
// alone, so the refresh analyzes every child, and the ANALYZE it names when
// it cannot run covers them as well.
func TestRunRefreshesTheStatisticsOfInheritanceChildren(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.p (id int, code varchar(50))",
		"CREATE TABLE %[1]s.c1 () INHERITS (%[1]s.p)",
		"CREATE TABLE %[1]s.c2 () INHERITS (%[1]s.c1)",
		"INSERT INTO %[1]s.c1 SELECT g, 'c' || (g %% 10) FROM generate_series(1, 500) g",
		"INSERT INTO %[1]s.c2 SELECT g, 'c' || (g %% 10) FROM generate_series(1, 500) g",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %s.p ALTER COLUMN code TYPE varchar(100)", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	require.NotNil(t, v.StatisticsRefresh)
	assert.True(t, v.StatisticsRefresh.Refreshed)
	for _, child := range []string{"c1", "c2"} {
		assert.True(t, hasTableColumnStatistics(t, pool, schema, child, "code"), "child %s is analyzed", child)
	}
}

// hasExpressionIndexStatistics reports whether the planner has statistics
// for the expression column of the named index.
func hasExpressionIndexStatistics(t *testing.T, pool *pgxpool.Pool, schema, index string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM pg_stats WHERE schemaname = $1 AND tablename = $2)",
		schema, index).Scan(&exists))
	return exists
}

// hasDependencyStatistics reports whether the named extended statistics
// object has its functional dependencies built.
func hasDependencyStatistics(t *testing.T, pool *pgxpool.Pool, schema, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM pg_stats_ext WHERE statistics_schemaname = $1 AND statistics_name = $2 AND dependencies IS NOT NULL)",
		schema, name).Scan(&exists))
	return exists
}

// hasTableColumnStatistics reports whether the planner has statistics for
// the column of the named table itself, not its inheritance tree.
func hasTableColumnStatistics(t *testing.T, pool *pgxpool.Pool, schema, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM pg_stats WHERE schemaname = $1 AND tablename = $2 AND attname = $3 AND NOT inherited)",
		schema, table, column).Scan(&exists))
	return exists
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
