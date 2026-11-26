package executor_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

// A concurrent detach whose first attempt commits its first transaction
// and then runs out of its lock budget waiting for an older transaction
// leaves the partition detach-pending; the retry finishes it with
// DETACH PARTITION ... FINALIZE. The step report says a FINALIZE ended the
// detach, though no earlier run left the partition pending.
func TestRunSequenceReportsTheFinalizeThatEndedItsOwnInterruptedDetach(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			day date NOT NULL,
			PRIMARY KEY (id, day)
		) PARTITION BY RANGE (day);
		CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
		CREATE TABLE %[1]s.events_2025 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2025-01-01') TO ('2026-01-01')`, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "events")

	reader, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	readerOpen := true
	endReader := func() {
		if !readerOpen {
			return
		}
		readerOpen = false
		defer reader.Release()
		_, err := reader.Exec(context.WithoutCancel(t.Context()), "COMMIT")
		assert.NoError(t, err, "end the reader")
	}
	t.Cleanup(endReader)
	_, err = reader.Exec(t.Context(), "BEGIN")
	require.NoError(t, err)
	_, err = reader.Exec(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.events", schema))
	require.NoError(t, err)

	budget := runBudget
	budget.Brief.LockTimeout = 300 * time.Millisecond
	// The backoff is the window in which the test ends the reader, between
	// the first attempt's failure and the retry.
	retry := executor.RetryPolicy{MaxAttempts: 2, InitialBackoff: 5 * time.Second, MaxBackoff: 5 * time.Second}
	type result struct {
		rep executor.SequenceReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := executor.RunSequence(t.Context(), pool, pt, []string{fmt.Sprintf(
			"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024 CONCURRENTLY", schema)}, budget, retry)
		done <- result{rep, err}
	}()

	const firstAttemptDeadline = 30 * time.Second
	require.Eventually(t, func() bool {
		var between bool
		err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (SELECT 1 FROM pg_inherits
			                WHERE inhrelid = (quote_ident($1) || '.events_2024')::regclass
			                  AND inhdetachpending)
			   AND NOT EXISTS (SELECT 1 FROM pg_stat_activity
			                    WHERE query LIKE 'ALTER TABLE % DETACH PARTITION %'
			                      AND state = 'active'
			                      AND pid <> pg_backend_pid())`, schema).Scan(&between)
		return err == nil && between
	}, firstAttemptDeadline, 25*time.Millisecond, "the first attempt must end with the partition detach-pending")
	endReader()

	res := <-done
	require.NoError(t, res.err)
	require.Len(t, res.rep.Steps, 1)
	assert.Equal(t, executor.StepDetachPartition, res.rep.Steps[0].Kind)
	assert.True(t, res.rep.Steps[0].FinishedPendingDetach, "the retry's FINALIZE ended the detach")
	var attached bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (SELECT 1 FROM pg_inherits
		                WHERE inhrelid = (quote_ident($1) || '.events_2024')::regclass)`, schema).Scan(&attached))
	assert.False(t, attached, "the partition is a table of its own")
}
