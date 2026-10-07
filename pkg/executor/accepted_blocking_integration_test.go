package executor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

	rep, err := executor.ExecuteAcceptedBlocking(t.Context(), pool, sql, pgx.Identifier{schema, "t"}, b)
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

	_, err = executor.ExecuteAcceptedBlocking(t.Context(), pool, fmt.Sprintf("DROP INDEX %s.i", schema), pgx.Identifier{schema, "t"}, b)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Less(t, time.Since(start), 2*time.Second)
	exists, valid := indexState(t, pool, schema, "i")
	assert.True(t, exists)
	assert.True(t, valid)
}

// REINDEX on a partitioned relation is admitted by shape but PostgreSQL
// refuses to run it inside a transaction block, which is the only way this
// executor runs anything. The failure is reported as a permanent outcome
// outside the path, not as a retryable execution failure, and nothing is
// changed: the parent and its partitions keep their indexes.
func TestExecuteAcceptedBlockingRefusesReindexOnPartitionedRelation(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.parent (id int) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.leaf PARTITION OF %[1]s.parent FOR VALUES FROM (0) TO (10);
		CREATE INDEX parent_i ON %[1]s.parent (id)`, schema))
	require.NoError(t, err)
	b := executor.BlockingBudget{LockTimeout: time.Second, StatementTimeout: 10 * time.Second}

	for _, sql := range []string{
		fmt.Sprintf("REINDEX TABLE %s.parent", schema),
		fmt.Sprintf("REINDEX INDEX %s.parent_i", schema),
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := executor.ExecuteAcceptedBlocking(t.Context(), pool, sql, pgx.Identifier{schema, "parent"}, b)
			require.ErrorIs(t, err, executor.ErrUnsupportedAcceptedBlocking)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, "25001", pgErr.Code)
			assert.True(t, executor.OutcomeCode(err).Permanent())
		})
	}
	exists, valid := indexState(t, pool, schema, "parent_i")
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
		fmt.Sprintf("CREATE INDEX observed_i ON %s.t (%s.observe_bounds(id))", schema, schema), pgx.Identifier{schema, "t"}, b)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "P0001", pgErr.Code, "the executing transaction must observe both exact bounds")
	var executionErr *executor.BlockingExecutionError
	assert.True(t, errors.As(err, &executionErr))
}

// A statement that acquires its own locks takes them one at a time, and
// statement_timeout counts all of those waits together while lock_timeout
// bounds each on its own. REINDEX INDEX locks the table, then the index:
// with a writer holding the table for a while and a reader holding the
// index for longer, the table wait is granted, the index wait runs past
// the statement bound before its own lock bound, and the cancellation
// would read as work that ran. The executor instead takes the table's
// ACCESS EXCLUSIVE lock as its own statement first, so the whole wait is
// one lock-budget wait: the outcome is a lock refusal, and the index
// stands.
func TestExecuteAcceptedBlockingLocksTheTableBeforeTheStatement(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id int PRIMARY KEY);
		CREATE INDEX orders_id_idx ON %[1]s.orders (id)`, schema))
	require.NoError(t, err)

	// The reader's open transaction keeps ACCESS SHARE on the table and on
	// every index planning opened, which conflicts with the ACCESS
	// EXCLUSIVE the reindex needs on the index but not with the SHARE it
	// needs on the table.
	reader, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Rollback(context.WithoutCancel(t.Context())) })
	var n int
	require.NoError(t, reader.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.orders WHERE id = 1", schema)).Scan(&n))

	// The writer's ROW EXCLUSIVE conflicts with the SHARE the reindex needs
	// on the table, so the table wait lasts until the writer commits.
	writer, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = writer.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.orders VALUES (1)", schema))
	require.NoError(t, err)
	const writerHolds = time.Second
	// The timer's goroutine owns the writer until it has committed; the
	// cleanup waits for that before its safety rollback so the two never
	// touch the transaction at once.
	committed := make(chan struct{})
	release := time.AfterFunc(writerHolds, func() {
		defer close(committed)
		_ = writer.Commit(context.WithoutCancel(t.Context()))
	})
	t.Cleanup(func() {
		if release.Stop() {
			close(committed)
		}
		<-committed
		_ = writer.Rollback(context.WithoutCancel(t.Context()))
	})

	// Each wait on its own fits the lock bound; the two in sequence do not
	// fit the statement bound.
	b := executor.BlockingBudget{LockTimeout: writerHolds + 500*time.Millisecond, StatementTimeout: writerHolds + 600*time.Millisecond}
	start := time.Now()
	_, err = executor.ExecuteAcceptedBlocking(t.Context(), pool,
		fmt.Sprintf("REINDEX INDEX %s.orders_id_idx", schema), pgx.Identifier{schema, "orders"}, b)
	elapsed := time.Since(start)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause, "the whole wait is the table lock's; nothing ran")
	assert.Equal(t, b.LockTimeout, budgetErr.Budget)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "55P03", pgErr.Code)
	assert.GreaterOrEqual(t, elapsed, b.LockTimeout, "the lock budget, not the writer's release, ended the wait")
	require.NoError(t, reader.Rollback(t.Context()))
	exists, valid := indexState(t, pool, schema, "orders_id_idx")
	assert.True(t, exists)
	assert.True(t, valid)
}

// The caller's context ending while the table lock is still being waited
// for is not an unknown outcome: the statement was never submitted, so
// the cancellation is reported as the caller's own and nothing changes.
func TestExecuteAcceptedBlockingReportsACancelledLockWaitAsNothingSubmitted(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int); CREATE INDEX i ON %s.t (id)", schema, schema))
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) })
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	b := executor.BlockingBudget{LockTimeout: 30 * time.Second, StatementTimeout: time.Minute}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := executor.ExecuteAcceptedBlocking(ctx, pool, fmt.Sprintf("DROP INDEX %s.i", schema), pgx.Identifier{schema, "t"}, b)
		done <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			  WHERE query LIKE 'LOCK TABLE %' AND wait_event_type = 'Lock')`).Scan(&waiting)
		return err == nil && waiting
	}, 30*time.Second, 25*time.Millisecond, "the table lock request must be waiting on the holder")
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, executor.ErrCancelledByCaller)
		var unknownErr *executor.BlockingOutcomeUnknownError
		assert.False(t, errors.As(err, &unknownErr), "nothing was submitted, so the outcome is known")
		assert.Equal(t, executor.CodeCancelledByCaller, executor.OutcomeCode(err))
	case <-time.After(30 * time.Second):
		t.Fatal("the cancelled lock wait must return")
	}
	require.NoError(t, holder.Rollback(t.Context()))
	exists, valid := indexState(t, pool, schema, "i")
	assert.True(t, exists)
	assert.True(t, valid)
}

// LOCK TABLE cannot name a materialized view, so REINDEX TABLE on one runs
// without the table lock taken first; the statement acquires its own locks
// and still commits under the budgets.
func TestExecuteAcceptedBlockingReindexesAMaterializedViewWithoutAPreLock(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id int PRIMARY KEY);
		CREATE MATERIALIZED VIEW %[1]s.order_ids AS SELECT id FROM %[1]s.orders;
		CREATE UNIQUE INDEX order_ids_id_idx ON %[1]s.order_ids (id)`, schema))
	require.NoError(t, err)
	b := executor.BlockingBudget{LockTimeout: time.Second, StatementTimeout: 10 * time.Second}

	_, err = executor.ExecuteAcceptedBlocking(t.Context(), pool, fmt.Sprintf("REINDEX TABLE %s.order_ids", schema), nil, b)
	require.NoError(t, err)
	exists, valid := indexState(t, pool, schema, "order_ids_id_idx")
	assert.True(t, exists)
	assert.True(t, valid)
}
