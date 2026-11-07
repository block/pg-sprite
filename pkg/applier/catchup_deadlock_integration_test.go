//go:build integration

package applier_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
)

// The flush and the copier's chunk passes both write the shadow, and
// while source changes move unique values between keys the two can each
// wait on a value the other's open transaction holds. The server breaks
// the cycle by rolling one back with deadlock_detected; when it picks the
// flush, the flush committed nothing, so the catch-up puts the batch back
// and flushes it again on its next cycle rather than ending.
//
// In both tests the other writer is an open transaction standing in for a
// chunk pass. It holds unique value 1000002 under key 9002, uncommitted,
// and the source gives uniq 1000001 to row 1 and 1000002 to row 2 in one
// transaction. The flush writes key 1 first, so it holds 1000001 when it
// reaches key 2 and waits on the writer; the writer then inserts 1000001
// under key 9001 and waits on the flush. The writer's deadlock check is
// put off for a minute while the flush keeps the server default, so the
// flush's check is the one that finds the cycle and the flush is the one
// rolled back.

// The writer rolls its rows back once the flush has been the victim, so
// the next cycle's flush lands both rows and the shadow converges, with
// the one deadlock on the record (CO-4).
func TestCatchupRequeuesAFlushRolledBackAsTheDeadlockVictim(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{positions: &switchPosition{pos: allLanded()}})
	writer := f.holdShadowValue(t, run)

	f.moveUniqueValues(t)
	writer.deadlockWithTheFlush(t)
	require.NoError(t, writer.tx.Rollback(t.Context()), "the writer's rows vanish, as a rolled-back chunk pass's would")

	end := f.currentWALLSN(t)
	run.awaitStatus(t, "flush both rows after the deadlock", func(s applier.Status) bool {
		return s.Confirmed >= end && s.Buffered == 0
	})
	s := run.status(t)
	assert.Equal(t, uint64(1), s.BatchesDeadlocked, "the one flush the server rolled back")
	assert.Equal(t, uint64(2), s.Applied, "both images land on the flush after it")
	assert.Zero(t, s.BatchesDeferred, "a deadlock is not a refusal")
	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// The writer keeps holding 1000002 and deadlocks every flush that reaches
// it, so the catch-up ends with ErrBatchDeadlocked, wrapping the server's
// deadlock_detected, once the bound of cycles in a row is reached.
func TestCatchupEndsWhenTheFlushIsTheDeadlockVictimEveryCycle(t *testing.T) {
	f := newCatchupFixture(t, 20)
	const attempts = 2
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{DeadlockAttempts: attempts},
	})
	writer := f.holdShadowValue(t, run)

	f.moveUniqueValues(t)
	for range attempts {
		writer.deadlockWithTheFlush(t)
		// Releasing 1000001 alone lets the next cycle's flush take it and
		// wait on 1000002 again, so the writer can deadlock that one too.
		require.NoError(t, writer.release(t.Context()))
	}

	err := run.wait(t)
	require.ErrorIs(t, err, applier.ErrBatchDeadlocked)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the server's error stays in the chain")
	assert.Equal(t, "40P01", pgErr.Code)
	assert.NotErrorIs(t, err, context.Canceled, "the catch-up ended on its own, not on a stop")
	assert.Equal(t, uint64(attempts-1), run.catchup.Status().BatchesDeadlocked, "the requeued victims are on the record; the one that ended the run is reported in the error")
	require.NoError(t, writer.tx.Rollback(t.Context()))
}

// moveUniqueValues gives rows 1 and 2 the unique values the flush will
// contend for, in one source transaction so both images share a batch.
func (f catchupFixture) moveUniqueValues(t *testing.T) {
	t.Helper()
	q := f.table.Qualified()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = 1000001 WHERE id = 1`)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = 1000002 WHERE id = 2`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
}

// shadowValueHolder is a transaction writing the shadow beside the flush.
type shadowValueHolder struct {
	// watch is a connection of its own for seeing the flush wait, acquired
	// before the contention starts so the check never queues behind the
	// flush for one of the pool's.
	watch      *pgxpool.Conn
	shadowName string
	tx         pgx.Tx
	pid        int
}

// holdShadowValue opens the writer's transaction and inserts unique value
// 1000002 under key 9002 without committing, with its deadlock check put
// off well past the flush's.
func (f catchupFixture) holdShadowValue(t *testing.T, run *catchupRun) *shadowValueHolder {
	t.Helper()
	watch, err := f.pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(watch.Release)
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Redundant safety closer: after the test's own rollback this
		// returns the guaranteed ErrTxClosed.
		_ = tx.Rollback(context.WithoutCancel(t.Context()))
	})
	w := &shadowValueHolder{
		watch:      watch,
		shadowName: pgx.Identifier{f.table.Schema, run.shadow.ShadowTable()}.Sanitize(),
		tx:         tx,
	}
	// The server checks a wait for a deadlock once, deadlock_timeout after
	// it starts, and rolls back the waiter whose check finds the cycle.
	_, err = tx.Exec(t.Context(), `SET LOCAL deadlock_timeout = '1min'`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&w.pid))
	_, err = tx.Exec(t.Context(), `INSERT INTO `+w.shadowName+` (id, uniq, amount, blob) VALUES (9002, 1000002, 0, NULL)`)
	require.NoError(t, err)
	return w
}

// deadlockWithTheFlush waits until the flush is blocked on the writer's
// value 1000002, then inserts 1000001 under key 9001, which the flush
// already holds, and returns once the server has rolled the flush back.
// The insert is made under a save point so release can give 1000001 back
// while 1000002 stays held.
func (w *shadowValueHolder) deadlockWithTheFlush(t *testing.T) {
	t.Helper()
	const blockedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		var blocked bool
		err := w.watch.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_stat_activity
				WHERE $1 = ANY (pg_catalog.pg_blocking_pids(pid))
			)`, w.pid).Scan(&blocked)
		return err == nil && blocked
	}, blockedDeadline, 10*time.Millisecond, "the flush should wait on value 1000002, which the writer holds")

	_, err := w.tx.Exec(t.Context(), `SAVEPOINT held_1000001`)
	require.NoError(t, err)
	_, err = w.tx.Exec(t.Context(), `INSERT INTO `+w.shadowName+` (id, uniq, amount, blob) VALUES (9001, 1000001, 0, NULL)`)
	require.NoError(t, err, "the server should roll the flush back, not the writer that began waiting later")
}

// release gives value 1000001 back by rolling the writer's save point
// back, leaving 1000002 held.
func (w *shadowValueHolder) release(ctx context.Context) error {
	_, err := w.tx.Exec(ctx, `ROLLBACK TO SAVEPOINT held_1000001`)
	return err
}
