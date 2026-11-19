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
// and flushes it again after a backoff rather than ending.
//
// In these tests the other writer is an open transaction standing in for
// a chunk pass. For rows 1 and 2 it holds unique value 1000002 under key
// 9002, uncommitted, and the source gives uniq 1000001 to row 1 and
// 1000002 to row 2 in one transaction. The flush writes key 1 first, so it
// holds 1000001 when it reaches key 2 and waits on the writer; the writer
// then inserts 1000001 under key 9001 and waits on the flush. The writer's
// deadlock check is put off for a minute while the flush keeps the server
// default, so the flush's check is the one that finds the cycle and the
// flush is the one rolled back. Rows 3 and 4 contend the same way for
// 1000003 and 1000004, under keys 9003 and 9004.

// rowsOneAndTwo and rowsThreeAndFour are the rows a round of contention
// moves unique values into, and the values.
var (
	rowsOneAndTwo    = contendedRows{firstID: 1, secondID: 2, first: 1000001, second: 1000002}
	rowsThreeAndFour = contendedRows{firstID: 3, secondID: 4, first: 1000003, second: 1000004}
)

// The writer rolls its rows back once the flush has been the victim, so
// the next cycle's flush lands both rows and the shadow converges, with
// the one deadlock on the record (CO-4). Until then the requeued batch's
// two entries wait in the buffer and count as deferred.
func TestCatchupRequeuesAFlushRolledBackAsTheDeadlockVictim(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{positions: &switchPosition{pos: allLanded()}})
	writer := f.holdShadowValue(t, run, rowsOneAndTwo)

	f.moveUniqueValues(t, rowsOneAndTwo)
	writer.deadlockWithTheFlush(t)
	// The next cycle's flush waits on the writer again, so no cycle
	// records anything after the deadlock's until the writer lets go.
	run.awaitStatus(t, "record the flush the server rolled back", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 1
	})
	assert.Equal(t, 2, run.status(t).Deferred, "both requeued entries wait in the buffer")
	require.NoError(t, writer.tx.Rollback(t.Context()), "the writer's rows vanish, as a rolled-back chunk pass's would")

	end := f.currentWALLSN(t)
	run.awaitStatus(t, "flush both rows after the deadlock", func(s applier.Status) bool {
		return s.Confirmed >= end && s.Buffered == 0
	})
	s := run.status(t)
	assert.Equal(t, uint64(1), s.BatchesDeadlocked, "the one flush the server rolled back")
	assert.Equal(t, uint64(2), s.Applied, "both images land on the flush after it")
	assert.Zero(t, s.BatchesDeferred, "a deadlock is not a refusal")
	assert.Zero(t, s.Deferred, "the flush that committed left nothing deferred")
	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// The bound counts deadlocks in a row: a flush that commits between two
// deadlocks starts the count again, so with a bound of two the second
// deadlock is requeued like the first rather than ending the catch-up.
func TestCatchupCountsDeadlocksAgainAfterAFlushCommits(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{DeadlockAttempts: 2},
	})

	first := f.holdShadowValue(t, run, rowsOneAndTwo)
	f.moveUniqueValues(t, rowsOneAndTwo)
	first.deadlockWithTheFlush(t)
	require.NoError(t, first.tx.Rollback(t.Context()))
	run.awaitStatus(t, "flush rows 1 and 2 after the first deadlock", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 1 && s.Applied == 2 && s.Buffered == 0
	})

	second := f.holdShadowValue(t, run, rowsThreeAndFour)
	f.moveUniqueValues(t, rowsThreeAndFour)
	second.deadlockWithTheFlush(t)
	require.NoError(t, second.tx.Rollback(t.Context()))

	end := f.currentWALLSN(t)
	run.awaitStatus(t, "flush rows 3 and 4 after the second deadlock", func(s applier.Status) bool {
		return s.Confirmed >= end && s.Buffered == 0
	})
	s := run.status(t)
	assert.Equal(t, uint64(2), s.BatchesDeadlocked, "both deadlocks were requeued, neither ended the run")
	assert.Equal(t, uint64(4), s.Applied, "rows 1 to 4 landed")
	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// A flush a constraint refuses also starts the count again: it was not a
// deadlock victim, so with a bound of two the deadlock after it is
// requeued like the one before it rather than ending the catch-up. The
// copy has a chunk in flight, so the refusal is requeued too, not the end
// of the run.
//
// The first writer deadlocks the flush over rows 1 and 2 and then commits
// its rows, which leaves stale shadow rows holding 1000001 and 1000002
// under keys 9001 and 9002: the retry is refused column-wise and whole-row
// (ErrBatchDeferred). A second writer deletes those rows without
// committing, so the next retry's upsert of key 1 locks row 1 and waits on
// the delete of the row under 9001; the second writer's update of row 1
// closes the cycle. Once the second writer commits its deletes the retry
// lands both rows (CO-4, CO-6).
func TestCatchupCountsDeadlocksAgainAfterAConstraintRefusesTheFlush(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: oneInFlight(t, 20)},
		opts:      applier.CatchupOptions{DeadlockAttempts: 2},
	})

	first := f.holdShadowValue(t, run, rowsOneAndTwo)
	f.moveUniqueValues(t, rowsOneAndTwo)
	first.deadlockWithTheFlush(t)
	run.awaitStatus(t, "record the first deadlock", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 1
	})
	require.NoError(t, first.tx.Commit(t.Context()), "the writer's rows stay in the shadow under keys the batch does not name")
	run.awaitStatus(t, "refuse the retry on the stale rows", func(s applier.Status) bool {
		return s.BatchesDeferred >= 1
	})

	second := f.openShadowWriter(t, run, rowsOneAndTwo)
	_, err := second.tx.Exec(t.Context(), `DELETE FROM `+second.shadowName+` WHERE id IN (9001, 9002)`)
	require.NoError(t, err)
	second.awaitBlockedFlush(t)
	_, err = second.tx.Exec(t.Context(), `SAVEPOINT locked_row`)
	require.NoError(t, err)
	_, err = second.tx.Exec(t.Context(), `UPDATE `+second.shadowName+` SET amount = amount WHERE id = 1`)
	require.NoError(t, err, "the server should roll the flush back, not the writer that began waiting later")
	run.awaitStatus(t, "record the second deadlock", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 2
	})
	_, err = second.tx.Exec(t.Context(), `ROLLBACK TO SAVEPOINT locked_row`)
	require.NoError(t, err)
	require.NoError(t, second.tx.Commit(t.Context()), "the stale rows are gone, as a chunk pass's resume clear would leave them")

	end := f.currentWALLSN(t)
	run.awaitStatus(t, "flush rows 1 and 2 once the stale rows are gone", func(s applier.Status) bool {
		return s.Confirmed >= end && s.Buffered == 0
	})
	s := run.status(t)
	assert.Equal(t, uint64(2), s.BatchesDeadlocked, "both deadlocks were requeued; the refusal between them reset the count")
	assert.Equal(t, uint64(2), s.Applied, "rows 1 and 2 landed")
	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// The writer keeps holding 1000002 and deadlocks every flush that reaches
// it, so the catch-up ends with ErrBatchDeadlocked, wrapping the server's
// deadlock_detected, once the bound of flushes in a row is reached.
func TestCatchupEndsWhenTheFlushIsTheDeadlockVictimEveryCycle(t *testing.T) {
	f := newCatchupFixture(t, 20)
	const attempts = 2
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{DeadlockAttempts: attempts},
	})
	writer := f.holdShadowValue(t, run, rowsOneAndTwo)

	f.moveUniqueValues(t, rowsOneAndTwo)
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

// contendedRows is two source rows, firstID below secondID so the flush
// writes firstID's image first, and the unique values a round gives them.
// The writer holds each value under key 9000 plus the row's id.
type contendedRows struct {
	firstID, secondID int
	first, second     int
}

// moveUniqueValues gives the rows the unique values the flush will
// contend for, in one source transaction so both images share a batch.
func (f catchupFixture) moveUniqueValues(t *testing.T, rows contendedRows) {
	t.Helper()
	q := f.table.Qualified()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = $1 WHERE id = $2`, rows.first, rows.firstID)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = $1 WHERE id = $2`, rows.second, rows.secondID)
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
	rows       contendedRows
	tx         pgx.Tx
	pid        int
}

// holdShadowValue opens the writer's transaction and inserts the second
// row's unique value under key 9000 plus its id without committing, with
// its deadlock check put off well past the flush's.
func (f catchupFixture) holdShadowValue(t *testing.T, run *catchupRun, rows contendedRows) *shadowValueHolder {
	t.Helper()
	w := f.openShadowWriter(t, run, rows)
	_, err := w.tx.Exec(t.Context(), `INSERT INTO `+w.shadowName+` (id, uniq, amount, blob) VALUES ($1, $2, 0, NULL)`, 9000+rows.secondID, rows.second)
	require.NoError(t, err)
	return w
}

// openShadowWriter opens the writer's transaction, with its deadlock check
// put off well past the flush's, and writes nothing yet.
func (f catchupFixture) openShadowWriter(t *testing.T, run *catchupRun, rows contendedRows) *shadowValueHolder {
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
		rows:       rows,
		tx:         tx,
	}
	// The server checks a wait for a deadlock once, deadlock_timeout after
	// it starts, and rolls back the waiter whose check finds the cycle.
	_, err = tx.Exec(t.Context(), `SET LOCAL deadlock_timeout = '1min'`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&w.pid))
	return w
}

// deadlockWithTheFlush waits until the flush is blocked on the writer's
// second value, then inserts the first value, which the flush already
// holds, under key 9000 plus the first row's id, and returns once the
// server has rolled the flush back. The insert is made under a save point
// so release can give the first value back while the second stays held.
func (w *shadowValueHolder) deadlockWithTheFlush(t *testing.T) {
	t.Helper()
	w.awaitBlockedFlush(t)
	_, err := w.tx.Exec(t.Context(), `SAVEPOINT held_first`)
	require.NoError(t, err)
	_, err = w.tx.Exec(t.Context(), `INSERT INTO `+w.shadowName+` (id, uniq, amount, blob) VALUES ($1, $2, 0, NULL)`, 9000+w.rows.firstID, w.rows.first)
	require.NoError(t, err, "the server should roll the flush back, not the writer that began waiting later")
}

// awaitBlockedFlush waits until the flush is blocked on the writer.
func (w *shadowValueHolder) awaitBlockedFlush(t *testing.T) {
	t.Helper()
	const blockedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		blocked, err := w.blocksAnyone(t.Context())
		return err == nil && blocked
	}, blockedDeadline, 10*time.Millisecond, "the flush should wait on the value the writer holds")
}

// blocksAnyone reports whether any backend waits on the writer; the flush
// is the only other writer of the shadow.
func (w *shadowValueHolder) blocksAnyone(ctx context.Context) (bool, error) {
	var blocked bool
	err := w.watch.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_stat_activity
			WHERE $1 = ANY (pg_catalog.pg_blocking_pids(pid))
		)`, w.pid).Scan(&blocked)
	return blocked, err
}

// release gives the first value back by rolling the writer's save point
// back, leaving the second held.
func (w *shadowValueHolder) release(ctx context.Context) error {
	_, err := w.tx.Exec(ctx, `ROLLBACK TO SAVEPOINT held_first`)
	return err
}
