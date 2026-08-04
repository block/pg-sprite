package executor_test

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
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
)

// budget is generous enough that an instant catalog change always fits and
// tight enough that a blocked or rewriting attempt is cancelled quickly.
var budget = executor.Budget{LockTimeout: 500 * time.Millisecond, StatementTimeout: 2 * time.Second}

func newPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, testutil.NewSchema(t, pool)
}

func mustPreflight(t *testing.T, pool *pgxpool.Pool, schema, table string) preflight.PreflightedTable {
	t.Helper()
	pt, err := preflight.CheckTable(t.Context(), pool, schema, table, 1<<30)
	require.NoError(t, err)
	return pt
}

func columnType(t *testing.T, pool *pgxpool.Pool, schema, table, column string) string {
	t.Helper()
	var typ string
	err := pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
		schema, table, column).Scan(&typ)
	require.NoError(t, err)
	return typ
}

func TestAttemptNativeCommitsInstantChange(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	sql := fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int NOT NULL DEFAULT 0", schema)
	require.NoError(t, executor.AttemptNative(t.Context(), pool, pt, sql, budget))

	assert.Equal(t, "integer", columnType(t, pool, schema, "t", "age"), "the committed change must be visible")
}

func TestAttemptNativeCancelsWhenLockBlocked(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// A second session holds ACCESS EXCLUSIVE for the whole test, so the
	// attempt can never be granted its lock.
	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	sql := fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema)
	err = executor.AttemptNative(t.Context(), pool, pt, sql, budget)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Equal(t, budget.LockTimeout, budgetErr.Budget)
}

func TestAttemptNativeCancelsRewriteAndLeavesTableUnchanged(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
	require.NoError(t, err)
	// Enough rows that a full table rewrite cannot finish inside a
	// millisecond-scale statement budget.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"INSERT INTO %s.t SELECT g, repeat('x', 100) FROM generate_series(1, 300000) g", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// int -> bigint forces a full table rewrite under ACCESS EXCLUSIVE.
	sql := fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema)
	tight := executor.Budget{LockTimeout: budget.LockTimeout, StatementTimeout: 50 * time.Millisecond}
	err = executor.AttemptNative(t.Context(), pool, pt, sql, tight)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseStatement, budgetErr.Cause)

	// The cancelled attempt must leave schema and data untouched.
	assert.Equal(t, "integer", columnType(t, pool, schema, "t", "id"))
	var count int
	require.NoError(t, pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&count))
	assert.Equal(t, 300000, count)
}

func TestAttemptNativeSurfacesOperationalErrors(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// Dropping a column that does not exist is a plain SQL error, not a
	// budget overrun.
	sql := fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN nope", schema)
	err = executor.AttemptNative(t.Context(), pool, pt, sql, budget)
	require.Error(t, err)
	var budgetErr *executor.BudgetError
	assert.NotErrorAs(t, err, &budgetErr)
}

func TestAttemptNativeRejectsUnboundedBudgets(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	sql := fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema)
	require.Error(t, executor.AttemptNative(t.Context(), pool, pt, sql, executor.Budget{LockTimeout: 0, StatementTimeout: time.Second}))
	require.Error(t, executor.AttemptNative(t.Context(), pool, pt, sql, executor.Budget{LockTimeout: time.Second, StatementTimeout: 0}))
}
