package executor_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

func TestExecuteAcceptedBlockingDropsIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int); CREATE INDEX i ON %s.t (id)", schema, schema))
	require.NoError(t, err)
	b := executor.BlockingBudget{LockTimeout: time.Second, StatementTimeout: 10 * time.Second}
	sql := fmt.Sprintf("DROP INDEX %s.i", schema)

	rep, err := executor.ExecuteAcceptedBlocking(t.Context(), pool, sql, b)
	require.NoError(t, err)
	assert.Equal(t, sql, rep.SQL)
	assert.Equal(t, b.LockTimeout, rep.LockTimeout)
	assert.Equal(t, b.StatementTimeout, rep.StatementTimeout)
	assert.Positive(t, rep.Duration)
	exists, _ := indexState(t, pool, schema, "i")
	assert.False(t, exists)
}

func TestExecuteAcceptedBlockingLockBudgetExecutesNothing(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int); CREATE INDEX i ON %s.t (id)", schema, schema))
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(t.Context()) })
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	b := executor.BlockingBudget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second}
	start := time.Now()

	_, err = executor.ExecuteAcceptedBlocking(t.Context(), pool, fmt.Sprintf("DROP INDEX %s.i", schema), b)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Less(t, time.Since(start), 2*time.Second)
	exists, valid := indexState(t, pool, schema, "i")
	assert.True(t, exists)
	assert.True(t, valid)
}

func TestExecuteAcceptedBlockingAppliesBothBoundsOnExecutingSession(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (id int);
		INSERT INTO %s.t VALUES (1);
		CREATE FUNCTION %s.observe_bounds(int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$
		BEGIN
			IF current_setting('lock_timeout') = '137ms' AND current_setting('statement_timeout') = '2468ms' THEN
				RAISE EXCEPTION USING ERRCODE = 'P0001';
			END IF;
			RAISE EXCEPTION USING ERRCODE = 'P0002';
		END $$`, schema, schema, schema))
	require.NoError(t, err)
	b := executor.BlockingBudget{LockTimeout: 137 * time.Millisecond, StatementTimeout: 2468 * time.Millisecond}

	_, err = executor.ExecuteAcceptedBlocking(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX observed_i ON %s.t (%s.observe_bounds(id))", schema, schema), b)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "P0001", pgErr.Code, "the executing transaction must observe both exact bounds")
	var executionErr *executor.BlockingExecutionError
	assert.True(t, errors.As(err, &executionErr))
}
