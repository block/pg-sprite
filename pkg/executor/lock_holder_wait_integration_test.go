package executor_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
)

// holderWaitDeadline bounds how long a test waits for a native attempt that
// was waiting outside the lock queue to finish once its holder is gone.
const holderWaitDeadline = 10 * time.Second

// createCounterTable creates schema.t with one row a writer can update.
func createCounterTable(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id int PRIMARY KEY,
			n  int NOT NULL
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.t VALUES (1, 0)", schema))
	require.NoError(t, err)
}

// openReader begins a transaction that reads schema.t and stays open, holding
// ACCESS SHARE on the table, the way a long report or an idle-in-transaction
// session does. It returns the transaction and its backend PID.
func openReader(t *testing.T, pool *pgxpool.Pool, schema string) (pgx.Tx, int32) {
	t.Helper()
	reader, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = reader.Rollback(context.WithoutCancel(t.Context()))
	})
	var count int
	require.NoError(t, reader.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&count))
	var pid int32
	require.NoError(t, reader.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid))
	return reader, pid
}

// writeUnderTightLockBudget updates schema.t in a transaction whose own
// lock_timeout is far below the attempt's, so the write fails if an ACCESS
// EXCLUSIVE request is queued ahead of it.
func writeUnderTightLockBudget(t *testing.T, pool *pgxpool.Pool, schema string) error {
	t.Helper()
	return pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL lock_timeout = '100ms'"); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), fmt.Sprintf("UPDATE %s.t SET n = n + 1 WHERE id = 1", schema))
		return err
	})
}

// queuedExclusiveRequest reports whether an ACCESS EXCLUSIVE request on
// schema.t is waiting in the lock queue.
func queuedExclusiveRequest(t *testing.T, pool *pgxpool.Pool, schema string) bool {
	t.Helper()
	var queued bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE relation = to_regclass($1)
			  AND mode = 'AccessExclusiveLock'
			  AND NOT granted
		)`, schema+".t").Scan(&queued))
	return queued
}

func hasColumn(t *testing.T, pool *pgxpool.Pool, schema, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 't' AND column_name = $2
		)`, schema, column).Scan(&exists))
	return exists
}

// awaitRun waits for a run started in the background to return.
func awaitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(holderWaitDeadline):
		require.FailNow(t, "the schema change did not finish after the reader ended")
		return nil
	}
}

// A reader holds a transaction open on the table while ADD COLUMN runs under
// the default retry policy. The step waits outside the lock queue instead of
// queueing behind the reader, so writers keep committing under a lock budget
// far shorter than the step's own and no ACCESS EXCLUSIVE request is ever
// seen waiting. Progress names the reader the step is waiting for. Once the
// reader ends, the column is added and the wait leaves progress.
func TestExecuteNativeWaitsOutsideTheLockQueueWhileAReaderHoldsTheTable(t *testing.T) {
	pool, schema := newPool(t)
	createCounterTable(t, pool, schema)
	pt := mustPreflight(t, pool, schema, "t")
	reader, readerPID := openReader(t, pool, schema)
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN coupon_code text", schema))
	// Without the wait this attempt would sit in the lock queue for its
	// whole two-second lock budget, and every write below would fail.
	wide := executor.Budget{LockTimeout: 2 * time.Second, StatementTimeout: 5 * time.Second}
	done := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		done <- executor.ExecuteNativeWithProgress(t.Context(), pool, pt, st, wide, executor.DefaultRetryPolicy(), tracker)
	})
	t.Cleanup(workers.Wait)

	for range 10 {
		require.NoError(t, writeUnderTightLockBudget(t, pool, schema),
			"a write must not queue behind the schema change while it waits for the reader")
		assert.False(t, queuedExclusiveRequest(t, pool, schema),
			"the schema change must wait outside the lock queue while the reader holds the table")
		time.Sleep(100 * time.Millisecond)
	}
	assert.False(t, hasColumn(t, pool, schema, "coupon_code"), "the column is added only after the reader ends")
	snap, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	wait := snap.Detail.LockHolderWait
	require.NotNil(t, wait, "progress reports the wait")
	assert.Equal(t, uint32(readerPID), wait.PID)
	assert.Equal(t, schema+".t", wait.Table)
	assert.Equal(t, executor.DefaultHolderWait, wait.Limit)
	assert.Positive(t, wait.Waited)

	require.NoError(t, reader.Rollback(t.Context()))
	require.NoError(t, awaitRun(t, done))
	assert.True(t, hasColumn(t, pool, schema, "coupon_code"))
	snap, err = tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, snap.Detail.LockHolderWait, "the wait leaves progress once it ends")
}

// A reader that keeps its transaction open past the whole wait ends the step
// before it requests its lock, so no reader or writer queues behind it: the
// error names the reader's backend and the table, the step returns without
// spending a lock budget, a write right after it is not held up, and the
// table is unchanged.
func TestExecuteNativeEndsWithoutQueueingWhenAReaderOutlastsTheWait(t *testing.T) {
	pool, schema := newPool(t)
	createCounterTable(t, pool, schema)
	pt := mustPreflight(t, pool, schema, "t")
	_, readerPID := openReader(t, pool, schema)

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN coupon_code text", schema))
	retry := executor.RetryPolicy{MaxAttempts: 3, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second, HolderWait: time.Second}
	began := time.Now()
	err := executor.ExecuteNative(t.Context(), pool, pt, st, budget, retry)
	elapsed := time.Since(began)

	var holderErr *executor.LingeringHolderError
	require.ErrorAs(t, err, &holderErr)
	assert.Equal(t, uint32(readerPID), holderErr.PID)
	assert.Equal(t, schema+".t", holderErr.Table)
	assert.GreaterOrEqual(t, holderErr.Held, retry.HolderWait-time.Second/2, "the reader held the table for the whole wait")
	assert.Contains(t, err.Error(), fmt.Sprintf("backend %d held a lock on %s.t", readerPID, schema))
	assert.False(t, errors.As(err, new(*executor.BudgetError)), "no attempt ran, so no lock budget was spent")
	assert.GreaterOrEqual(t, elapsed, retry.HolderWait, "the step waited out the whole wait")
	assert.Less(t, elapsed, retry.HolderWait+budget.LockTimeout, "the step never requested its lock")
	require.NoError(t, writeUnderTightLockBudget(t, pool, schema), "nothing is queued ahead of a writer")
	assert.False(t, hasColumn(t, pool, schema, "coupon_code"))
}

// Short write transactions keep committing on the table the whole time ADD
// COLUMN runs under the default retry policy. Each holds its lock for far
// less than the gap between two reads of the table's lock holders, so none
// is taken for a long holder and the column is added.
func TestExecuteNativeHolderWaitLetsShortTransactionsThrough(t *testing.T) {
	pool, schema := newPool(t)
	createCounterTable(t, pool, schema)
	pt := mustPreflight(t, pool, schema, "t")

	ctx, stop := context.WithCancel(t.Context())
	var writers sync.WaitGroup
	writers.Go(func() {
		for ctx.Err() == nil {
			// A write that times out behind the attempt's own brief
			// ACCESS EXCLUSIVE window is fine; the loop only keeps short
			// transactions on the table.
			_, _ = pool.Exec(ctx, fmt.Sprintf("UPDATE %s.t SET n = n + 1 WHERE id = 1", schema))
		}
	})
	t.Cleanup(func() {
		stop()
		writers.Wait()
	})

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN coupon_code text", schema))
	require.NoError(t, executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy()))
	assert.True(t, hasColumn(t, pool, schema, "coupon_code"))
}

// A reviewed table drop waits outside the lock queue the same way while a
// reader holds the table, so writers are not held behind it, and drops the
// table once the reader ends.
func TestExecuteDropTableWaitsOutsideTheLockQueueWhileAReaderHoldsTheTable(t *testing.T) {
	pool, schema := newPool(t)
	createCounterTable(t, pool, schema)
	definition := plannedTableDefinition(t, pool, schema, "t")
	reader, _ := openReader(t, pool, schema)

	wide := executor.Budget{LockTimeout: 2 * time.Second, StatementTimeout: 5 * time.Second}
	done := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		done <- executor.ExecuteDropTable(t.Context(), pool, schema, "t", definition, wide, executor.DefaultRetryPolicy())
	})
	t.Cleanup(workers.Wait)

	for range 5 {
		require.NoError(t, writeUnderTightLockBudget(t, pool, schema),
			"a write must not queue behind the drop while it waits for the reader")
		assert.False(t, queuedExclusiveRequest(t, pool, schema))
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, reader.Rollback(t.Context()))
	require.NoError(t, awaitRun(t, done))
	assert.False(t, relationExists(t, pool, schema, "t"))
}
