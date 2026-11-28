package migrate_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// leaveDetachPending leaves schema.table detach-pending under
// schema.events: a concurrent detach run directly on a session of its own,
// with a short lock_timeout, commits its first transaction and is then
// cancelled while it waits for a reader's transaction.
func leaveDetachPending(t *testing.T, pool *pgxpool.Pool, schema, table string) {
	t.Helper()
	endReader, _ := openReader(t, pool, schema)
	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(t.Context(), "SET lock_timeout = '300ms'")
	require.NoError(t, err)
	_, err = conn.Exec(t.Context(), fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.%[2]s CONCURRENTLY", schema, table))
	require.Error(t, err, "the reader outlasts the lock_timeout")
	_, err = conn.Exec(t.Context(), "RESET lock_timeout")
	require.NoError(t, err)
	endReader()
	attached, pending := partitionState(t, pool, schema, table)
	require.True(t, attached)
	require.True(t, pending, "the setup leaves the partition detach-pending")
}

// PostgreSQL detaches one partition of a table at a time. With events_2024
// left detach-pending by a detach run outside pg-sprite, a detach of
// events_2025 is refused before anything runs, with the code that names a
// pending detach to finish. Running the statement that detaches
// events_2024 finishes it, and the detach of events_2025 then runs.
func TestRunRefusesADetachWhileAnotherPartitionIsPending(t *testing.T) {
	pool := detachPool(t)
	schema := seedEvents(t, pool)
	leaveDetachPending(t, pool, schema, "events_2024")

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2025", schema)), detachOptions(schema))
	require.ErrorIs(t, err, executor.ErrPartitionDetachPending)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodePartitionDetachPending), v.Code)
	attached, pending := partitionState(t, pool, schema, "events_2025")
	assert.True(t, attached, "nothing ran on events_2025")
	assert.False(t, pending, "nothing ran on events_2025")

	v, err = migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024", schema)), detachOptions(schema))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	v, err = migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2025", schema)), detachOptions(schema))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	attached, _ = partitionState(t, pool, schema, "events_2025")
	assert.False(t, attached)
}

// A caller that cancels a concurrent detach while it waits for an older
// transaction gets the interrupted code, not a generic failure: the state
// is read again after the cancellation, and the partition is
// detach-pending. Running the same statement again finishes it.
func TestRunReportsADetachCancelledWhileItWaitsAsInterrupted(t *testing.T) {
	pool := detachPool(t)
	schema := seedEvents(t, pool)
	sql := fmt.Sprintf("ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024 CONCURRENTLY", schema)
	opts := detachOptions(schema)
	opts.Budget.Brief.LockTimeout = 30 * time.Second
	opts.Budget.Brief.StatementTimeout = time.Minute
	endReader, reader := openReader(t, pool, schema)

	type result struct {
		v   verdict.Verdict
		err error
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	st := parseOne(t, sql)
	done := make(chan result, 1)
	go func() {
		v, err := migrate.Run(ctx, pool, st, opts)
		done <- result{v, err}
	}()
	waitForDetachToWait(t, pool, reader)
	cancel()
	res := <-done
	require.ErrorIs(t, res.err, executor.ErrDetachInterrupted)
	assert.Equal(t, verdict.OutcomeFailed, res.v.Outcome)
	assert.Equal(t, string(executor.CodeDetachInterrupted), res.v.Code)
	attached, pending := partitionState(t, pool, schema, "events_2024")
	assert.True(t, attached)
	assert.True(t, pending, "the first transaction committed before the cancellation")
	const cancelDeadline = 30 * time.Second
	require.Eventually(t, func() bool {
		var running bool
		err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			 WHERE query LIKE 'ALTER TABLE % DETACH PARTITION %'
			   AND strpos(query, $1) > 0
			   AND state = 'active'
			   AND pid <> pg_backend_pid())`, schema).Scan(&running)
		return err == nil && !running
	}, cancelDeadline, 25*time.Millisecond, "the cancellation must stop the detach on the server")
	endReader()

	v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	attached, _ = partitionState(t, pool, schema, "events_2024")
	assert.False(t, attached, "the rerun finished the detach")
}

// The statement budget bounds the whole detach, its wait for older
// transactions included: with a lock budget far longer than the statement
// budget, the statement budget ends the wait, and the partition is left
// detach-pending with the interrupted code.
func TestRunEndsADetachAtItsStatementBudget(t *testing.T) {
	pool := detachPool(t)
	schema := seedEvents(t, pool)
	opts := detachOptions(schema)
	opts.Budget.Brief.LockTimeout = 30 * time.Second
	opts.Budget.Brief.StatementTimeout = 500 * time.Millisecond
	endReader, _ := openReader(t, pool, schema)

	start := time.Now()
	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024 CONCURRENTLY", schema)), opts)
	elapsed := time.Since(start)
	require.ErrorIs(t, err, executor.ErrDetachInterrupted)
	assert.Equal(t, string(executor.CodeDetachInterrupted), v.Code)
	assert.Less(t, elapsed, opts.Budget.Brief.LockTimeout, "the statement budget, not the lock budget, ended the wait")
	attached, pending := partitionState(t, pool, schema, "events_2024")
	assert.True(t, attached)
	assert.True(t, pending)
	endReader()
}

// A detach whose lock budget runs out before its first transaction commits
// leaves nothing behind: a transaction holding SHARE UPDATE EXCLUSIVE on the
// table outlasts every bounded attempt, the run is refused as over its
// budget with nothing executed, and the partition is neither detached nor
// pending.
func TestRunLeavesADetachThatNeverTookItsLockAttached(t *testing.T) {
	pool := detachPool(t)
	schema := seedEvents(t, pool)
	opts := detachOptions(schema)
	opts.Budget.Brief.LockTimeout = 300 * time.Millisecond
	opts.Retry = executor.RetryPolicy{MaxAttempts: 2, InitialBackoff: 50 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.events IN SHARE UPDATE EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024 CONCURRENTLY", schema)), opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Equal(t, 2, v.Attempts, "each bounded attempt waited out its lock budget")
	attached, pending := partitionState(t, pool, schema, "events_2024")
	assert.True(t, attached)
	assert.False(t, pending, "the first transaction never committed")
}
