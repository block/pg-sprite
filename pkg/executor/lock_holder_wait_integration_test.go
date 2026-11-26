package executor_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
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

// A reader holds a transaction open on the table while ADD COLUMN runs with a
// holder wait. The attempt waits outside the lock queue instead of queuing
// behind the reader, so writers keep committing under a lock budget far
// shorter than the attempt's own, and no ACCESS EXCLUSIVE request is ever
// seen waiting. Once the reader ends, the column is added.
func TestExecuteNativeWaitsOutsideTheLockQueueWhileAReaderHoldsTheTable(t *testing.T) {
	pool, schema := newPool(t)
	createCounterTable(t, pool, schema)
	pt := mustPreflight(t, pool, schema, "t")
	reader, _ := openReader(t, pool, schema)

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN coupon_code text", schema))
	// Without the holder wait this attempt would sit in the lock queue for
	// its whole two-second lock budget, and every write below would fail.
	wide := executor.Budget{LockTimeout: 2 * time.Second, StatementTimeout: 5 * time.Second}
	retry := executor.RetryPolicy{MaxAttempts: 1, HolderWait: holderWaitDeadline}
	done := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		done <- executor.ExecuteNative(t.Context(), pool, pt, st, wide, retry)
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

	require.NoError(t, reader.Rollback(t.Context()))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(holderWaitDeadline):
		require.FailNow(t, "the schema change did not finish after the reader ended")
	}
	assert.True(t, hasColumn(t, pool, schema, "coupon_code"))
}

// A reader that keeps its transaction open past the whole holder wait ends
// the run with a LockHolderWaitError naming the reader's backend. The
// statement was never submitted, so the table is unchanged.
func TestExecuteNativeReportsALockHolderThatOutlastsTheWait(t *testing.T) {
	pool, schema := newPool(t)
	createCounterTable(t, pool, schema)
	pt := mustPreflight(t, pool, schema, "t")
	_, readerPID := openReader(t, pool, schema)

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN coupon_code text", schema))
	retry := executor.RetryPolicy{MaxAttempts: 1, HolderWait: time.Second}
	err := executor.ExecuteNative(t.Context(), pool, pt, st, budget, retry)

	var holderErr *executor.LockHolderWaitError
	require.ErrorAs(t, err, &holderErr)
	assert.Equal(t, readerPID, holderErr.PID)
	assert.Equal(t, schema, holderErr.Schema)
	assert.Equal(t, "t", holderErr.Table)
	assert.GreaterOrEqual(t, holderErr.Waited, retry.HolderWait)
	assert.False(t, hasColumn(t, pool, schema, "coupon_code"))
}

// Short write transactions keep committing on the table the whole time ADD
// COLUMN runs with a holder wait. Each holds its lock for far less than the
// gap between two reads of the table's lock holders, so none is taken for a
// long holder and the column is added.
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
	retry := executor.RetryPolicy{MaxAttempts: 1, HolderWait: holderWaitDeadline}
	require.NoError(t, executor.ExecuteNative(t.Context(), pool, pt, st, budget, retry))
	assert.True(t, hasColumn(t, pool, schema, "coupon_code"))
}
