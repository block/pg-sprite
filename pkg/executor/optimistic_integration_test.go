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
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
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

func mustParse(t *testing.T, sql string) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	return st
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

func TestExecuteNativeCommitsInstantChange(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int NOT NULL DEFAULT 0", schema))
	require.NoError(t, executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy()))

	assert.Equal(t, "integer", columnType(t, pool, schema, "t", "age"), "the committed change must be visible")
}

func TestExecuteNativeCancelsWhenLockBlocked(t *testing.T) {
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

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	err = executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Equal(t, budget.LockTimeout, budgetErr.Budget)
	assert.Equal(t, executor.DefaultRetryPolicy().MaxAttempts, budgetErr.Attempts,
		"an actually blocked DDL must exhaust the bounded retry policy")
}

func TestExecuteNativeCancelsRewriteAndLeavesTableUnchanged(t *testing.T) {
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
	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	tight := executor.Budget{LockTimeout: budget.LockTimeout, StatementTimeout: 50 * time.Millisecond}
	err = executor.ExecuteNative(t.Context(), pool, pt, st, tight, executor.DefaultRetryPolicy())

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

func TestExecuteNativeSurfacesOperationalErrors(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// Dropping a column that does not exist is a plain SQL error, not a
	// budget overrun.
	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN nope", schema))
	err = executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())
	require.Error(t, err)
	var budgetErr *executor.BudgetError
	assert.NotErrorAs(t, err, &budgetErr)
}

func TestExecuteNativeWithProgressCommitsAndFinishes(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int NOT NULL DEFAULT 0", schema))
	require.NoError(t, executor.ExecuteNativeWithProgress(t.Context(), pool, pt, st, budget,
		executor.DefaultRetryPolicy(), tracker))

	assert.Equal(t, "integer", columnType(t, pool, schema, "t", "age"), "the committed change must be visible")
	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, snapshot.Phase)
	assert.Equal(t, 1, snapshot.Step)
	assert.Equal(t, 1, snapshot.TotalSteps)
	assert.Equal(t, progress.OperationOptimistic, snapshot.Detail.Operation)
	assert.Equal(t, 1, snapshot.Detail.Attempt, "the one successful attempt must be observed")
	assert.False(t, snapshot.Detail.Active)
}

// A blocked attempt exhausts its bounded retries; the tracker must report
// every retry attempt as it runs and a failed terminal phase at the end.
func TestExecuteNativeWithProgressReportsRetriesAndFailure(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	// A second session holds ACCESS EXCLUSIVE for the whole test, so the
	// attempt can never be granted its lock.
	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	retry := executor.RetryPolicy{MaxAttempts: 2, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}
	tight := executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: time.Second}
	err = executor.ExecuteNativeWithProgress(t.Context(), pool, pt, st, tight, retry, tracker)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFailed, snapshot.Phase)
	assert.Equal(t, retry.MaxAttempts, snapshot.Detail.Attempt,
		"the tracker must have observed the final bounded attempt")
	assert.False(t, snapshot.Detail.Active)
}

// Sub-millisecond budgets are as unbounded as zero ones: they truncate to
// PostgreSQL's 0ms, which disables the corresponding limit entirely.
func TestExecuteNativeRejectsUnboundedBudgets(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	unbounded := map[string]executor.Budget{
		"zero lock":                 {LockTimeout: 0, StatementTimeout: time.Second},
		"zero statement":            {LockTimeout: time.Second, StatementTimeout: 0},
		"sub-millisecond lock":      {LockTimeout: 500 * time.Microsecond, StatementTimeout: time.Second},
		"sub-millisecond statement": {LockTimeout: time.Second, StatementTimeout: 999 * time.Microsecond},
	}
	for name, b := range unbounded {
		t.Run(name, func(t *testing.T) {
			require.Error(t, executor.ExecuteNative(t.Context(), pool, pt, st, b, executor.DefaultRetryPolicy()))
		})
	}

	// The smallest representable budget is valid and does not serialize to 0:
	// a 1ms lock timeout must still cancel a blocked attempt rather than
	// disabling the limit.
	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	err = executor.ExecuteNative(t.Context(), pool, pt, st, executor.Budget{LockTimeout: time.Millisecond, StatementTimeout: time.Second}, executor.DefaultRetryPolicy())
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
}

// INV: ST-7 — a preflight proof for one table can never execute a statement
// against another, and a statement without a table target never executes.
func TestExecuteNativeRefusesTargetMismatch(t *testing.T) {
	pool, schema := newPool(t)
	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema),
		fmt.Sprintf("CREATE TABLE %s.victim (id int PRIMARY KEY)", schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}
	pt := mustPreflight(t, pool, schema, "t")

	t.Run("statement targets a different table", func(t *testing.T) {
		st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.victim ADD COLUMN a int", schema))
		err := executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())
		require.ErrorIs(t, err, executor.ErrInvariantViolation)

		var n int
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 'victim' AND column_name = 'a'`, schema).Scan(&n))
		assert.Zero(t, n, "the refused statement must never reach the database")
	})

	t.Run("unqualified statement does not match a qualified proof", func(t *testing.T) {
		// Fail-closed: the proof verified schema.t, the statement names a
		// bare t that search_path could resolve elsewhere.
		st := mustParse(t, "ALTER TABLE t ADD COLUMN a int")
		err := executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())
		require.ErrorIs(t, err, executor.ErrInvariantViolation)
	})

	t.Run("statement without a table target", func(t *testing.T) {
		st := mustParse(t, "CREATE TABLE elsewhere (id int)")
		err := executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())
		require.ErrorIs(t, err, executor.ErrInvariantViolation)
	})
}
