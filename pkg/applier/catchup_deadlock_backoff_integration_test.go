package applier_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
)

// deadlockTestBackoff is the deadlock backoff the test here sets: far
// longer than the test runs, so no retry comes due unless the test moves
// the catch-up's clock past it.
const deadlockTestBackoff = time.Hour

// A batch the server rolled back as the deadlock victim waits one backoff
// before its first retry and two before its second, measured on the
// catch-up's clock, so a run of deadlocks spreads its retries out instead
// of spending DeadlockAttempts within a few cycles. While it waits the
// cycles go on reading and confirming the stream, and no flush reaches the
// writer that still holds the contended value.
func TestCatchupWaitsOneBackoffLongerBeforeEachDeadlockRetry(t *testing.T) {
	f := newCatchupFixture(t, 20)
	clock := &shiftedClock{}
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{Clock: clock, DeadlockBackoff: deadlockTestBackoff},
	})
	writer := f.holdShadowValue(t, run, rowsOneAndTwo)
	f.moveUniqueValues(t, rowsOneAndTwo)

	writer.deadlockWithTheFlush(t)
	run.awaitStatus(t, "record the first flush the server rolled back", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 1
	})
	// Giving 1000001 back lets a retry take it and wait on 1000002 again,
	// so a retry is seen as the flush waiting on the writer.
	require.NoError(t, writer.release(t.Context()))
	run.awaitCyclesWithoutAFlush(t, writer, "before the first deadlock's backoff of one has passed")

	clock.advance(deadlockTestBackoff)
	writer.deadlockWithTheFlush(t)
	run.awaitStatus(t, "record the second flush the server rolled back", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 2
	})
	require.NoError(t, writer.release(t.Context()))
	clock.advance(deadlockTestBackoff)
	run.awaitCyclesWithoutAFlush(t, writer, "one backoff into the second deadlock's backoff of two")

	clock.advance(deadlockTestBackoff)
	writer.awaitBlockedFlush(t)
	require.NoError(t, writer.tx.Rollback(t.Context()), "the writer's rows vanish, as a rolled-back chunk pass's would")

	end := f.currentWALLSN(t)
	run.awaitStatus(t, "flush both rows once the second backoff has passed", func(s applier.Status) bool {
		return s.Confirmed >= end && s.Buffered == 0
	})
	s := run.status(t)
	assert.Equal(t, uint64(2), s.BatchesDeadlocked, "the two flushes the server rolled back")
	assert.Equal(t, uint64(2), s.Applied, "both images land on the retry after the second backoff")
	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// While a deadlocked batch waits out its backoff, the status says the
// catch-up is backing off and keeps saying its entries wait: Deferred
// counts them, Buffered holds them, Applied stands still, and the confirmed
// position stays below them while the stream delivers past it. A cancel
// inside an hour-long backoff still ends the catch-up within the stop
// deadline, since a backing-off cycle waits on nothing but its Interval.
func TestCatchupStatusKeepsTheRequeuedEntriesWhileBackingOff(t *testing.T) {
	f := newCatchupFixture(t, 20)
	clock := &shiftedClock{}
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{Clock: clock, DeadlockBackoff: deadlockTestBackoff},
	})
	writer := f.holdShadowValue(t, run, rowsOneAndTwo)
	f.moveUniqueValues(t, rowsOneAndTwo)

	writer.deadlockWithTheFlush(t)
	run.awaitStatus(t, "record the flush the server rolled back", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 1
	})
	rolledBack := run.status(t)
	require.NoError(t, writer.release(t.Context()))
	run.awaitCyclesWithoutAFlush(t, writer, "inside the first backoff")

	s := run.status(t)
	assert.True(t, s.BackingOff, "the cycle waited out the backoff instead of flushing")
	assert.False(t, s.FlushesHeld, "no Pause holds the flushes; the backoff does")
	assert.Equal(t, 2, s.Buffered, "the two requeued entries wait in the buffer")
	assert.Equal(t, 2, s.Deferred, "the two requeued entries still count as waiting")
	assert.Zero(t, s.Applied)
	assert.Equal(t, rolledBack.Confirmed, s.Confirmed, "the confirmed position stays below the requeued batch")
	assert.Greater(t, s.Delivered, s.Confirmed, "the stream delivered the marks past the requeued batch")
	run.stop(t)
	require.NoError(t, writer.tx.Rollback(t.Context()))
}

// A deadlocked batch waiting out its backoff holds the confirmation below
// it, so on a busy source the slot can reach the ceiling before the
// deadlocks use up their attempts. The stop is the ceiling's, at once, as
// the stream falling behind, and it carries the deadlocks in progress —
// one flush rolled back, its batch still backing off — so a host can name
// both causes. The backoff is an hour, so no retry comes due before the
// committed rows take the unconfirmed WAL over the small ceiling.
func TestCatchupLagStopDuringADeadlockBackoffCarriesTheDeadlocks(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{SlotLagCeiling: slotLagTestCeiling, DeadlockBackoff: deadlockTestBackoff},
	})
	writer := f.holdShadowValue(t, run, rowsOneAndTwo)
	f.moveUniqueValues(t, rowsOneAndTwo)

	writer.deadlockWithTheFlush(t)
	run.awaitStatus(t, "back off after the flush the server rolled back", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 1 && s.BackingOff
	})

	// Several MiB of committed rows: the catch-up buffers them while it
	// backs off and cannot confirm past the requeued batch below them.
	_, err := f.pool.Exec(t.Context(), `
		INSERT INTO `+f.table.Qualified()+` (uniq, amount, label, blob)
		SELECT -g, 0, 'busy', repeat('w', 1024)
		FROM generate_series(1, 4096) AS g`)
	require.NoError(t, err)

	err = run.wait(t)
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	assert.NotErrorIs(t, err, applier.ErrBatchDeadlocked, "the ceiling stopped the run before the deadlocks used up their attempts")
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, decode.SlotLagStreamBehind, exceeded.Cause, "the backoff held the confirmation below the requeued batch")
	assert.Greater(t, exceeded.Unconfirmed, slotLagTestCeiling)
	assert.Equal(t, decode.CatchupDeadlocks{Consecutive: 1, BackingOff: true}, exceeded.Deadlocks,
		"the stop names the deadlock in progress and the backoff it was waiting out")
	require.NoError(t, writer.tx.Rollback(t.Context()))
}

// awaitCyclesWithoutAFlush waits for the catch-up to complete a whole cycle
// after the call, failing the test the moment a flush waits on the writer
// instead. Each mark is a WAL record written outside the published table,
// so a cycle has measured past it only once it ended after the record;
// two marks in turn make one cycle's drain and flush fall between them.
func (r *catchupRun) awaitCyclesWithoutAFlush(t *testing.T, writer *shadowValueHolder, when string) {
	t.Helper()
	const cycleDeadline = 30 * time.Second
	deadline := time.NewTimer(cycleDeadline)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for range 2 {
		_, err := r.f.pool.Exec(t.Context(), `SELECT pg_catalog.pg_logical_emit_message(true, 'pg-sprite-test-mark', '')`)
		require.NoError(t, err)
		mark := r.f.currentWALLSN(t)
		for r.status(t).WALEnd < mark {
			blocked, err := writer.blocksAnyone(t.Context())
			require.NoError(t, err)
			require.False(t, blocked, "the catch-up retried the deadlocked batch %s", when)
			select {
			case <-deadline.C:
				t.Fatalf("no cycle measured past %s within %s; last status %+v", mark, cycleDeadline, r.status(t))
			case <-poll.C:
			}
		}
	}
}
