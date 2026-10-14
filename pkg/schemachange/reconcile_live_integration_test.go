package schemachange_test

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The test here runs reconcile mode against a source that is being written
// the whole time: the application does not pause for a reconcile, so a
// recopied chunk a write lands in afterwards is settled by the reopened
// stream, not refused by the pass.

// sourceWriter writes the workload table on a fixed interval until stopped:
// each tick updates one seeded row, cycling through every chunk, and
// inserts one row beyond the seeded keys. A stop is observed between ticks,
// never inside a statement: each statement runs detached from the writer's
// context under its own bound, so the tick in flight when the stop lands
// finishes both its statements and its writes are committed, or counted
// as a failure, before stop returns. Nothing the writer did is left
// undecided on the server once it has stopped.
type sourceWriter struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	done   chan struct{} // closed when the writer's goroutine has exited
	err    error
	writes int
}

// writerStatementBound is the time one of the writer's statements may take;
// a statement blocked for longer is a failure the writer records, so a stop
// is never held behind a lock indefinitely.
const writerStatementBound = 2 * time.Second

// startWriter runs a sourceWriter over rows seeded keys. The caller stops it
// with stop; the fixture's cleanup stops it too.
func (f reconcileFixture) startWriter(t *testing.T, seeded int, interval time.Duration) *sourceWriter {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	w := &sourceWriter{cancel: cancel, done: make(chan struct{})}
	update := fmt.Sprintf(`UPDATE %s SET amount = amount + 1 WHERE id = $1`, f.table.Qualified())
	insert := fmt.Sprintf(`INSERT INTO %s (uniq, amount, label, blob) VALUES ($1, 1, 'live', 'x')`, f.table.Qualified())
	exec := func(what, sql string, arg any) bool {
		stmtCtx, done := context.WithTimeout(context.WithoutCancel(ctx), writerStatementBound)
		defer done()
		if _, err := f.pool.Exec(stmtCtx, sql, arg); err != nil {
			w.err = fmt.Errorf("%s: %w", what, err)
			return false
		}
		return true
	}
	w.wg.Go(func() {
		defer close(w.done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			// A tick that was pending while a statement waited does not
			// outrank a stop that landed in the same window.
			if ctx.Err() != nil {
				return
			}
			if !exec("live update", update, int64(i%seeded)+1) || !exec("live insert", insert, 100001+i) {
				return
			}
			w.writes++
		}
	})
	t.Cleanup(func() { cancel(); w.wg.Wait() })
	return w
}

// stop ends the writer, waits for the tick in flight to finish, and returns
// how many ticks wrote both statements; a writer that failed fails the test.
func (w *sourceWriter) stop(t *testing.T) int {
	t.Helper()
	w.cancel()
	w.wg.Wait()
	require.NoError(t, w.err)
	return w.writes
}

// awaitBlocked waits until a session in the fixture's database is waiting
// on a lock, which is how a test knows the writer's statement is in flight
// and held rather than merely not started yet.
func (f reconcileFixture) awaitBlocked(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&n))
		return n > 0
	}, 20*time.Second, 20*time.Millisecond, "the writer never blocked on the held lock")
}

// liveRows counts the rows the writer inserted.
func (f reconcileFixture) liveRows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s WHERE label = 'live'`, f.table.Qualified())).Scan(&n))
	return n
}

// holdLock opens a transaction that holds the workload table in ACCESS
// EXCLUSIVE mode until the returned release is called, so the writer's next
// statement is guaranteed to be in flight and waiting.
func (f reconcileFixture) holdLock(t *testing.T) (release func()) {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s IN ACCESS EXCLUSIVE MODE`, f.table.Qualified()))
	require.NoError(t, err)
	var once sync.Once
	release = func() { once.Do(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) }) }
	t.Cleanup(release)
	return release
}

// A stop that lands while a statement is blocked is not a failure: the
// statement is not cancelled with the writer, it waits for the lock, and
// once the lock is released the tick completes and its writes are
// committed before stop returns.
func TestWriterStoppedInsideABlockedUpdateFinishesTheTick(t *testing.T) {
	const seeded = 50
	f := newReconcileFixture(t, seeded)
	release := f.holdLock(t)
	w := f.startWriter(t, seeded, 5*time.Millisecond)
	f.awaitBlocked(t)
	w.cancel()
	release()
	assert.Equal(t, 1, w.stop(t), "the tick in flight at the stop completed")
	assert.Equal(t, 1, f.liveRows(t), "the blocked tick's insert is committed, not left undecided")
}

// The same for a stop that lands while the insert half of a tick is blocked
// on an uncommitted row with its unique key.
func TestWriterStoppedInsideABlockedInsertFinishesTheTick(t *testing.T) {
	const seeded = 50
	f := newReconcileFixture(t, seeded)
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) })
	_, err = tx.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s (uniq, amount, label, blob) VALUES (100001, 1, 'held', 'x')`, f.table.Qualified()))
	require.NoError(t, err)
	w := f.startWriter(t, seeded, 5*time.Millisecond)
	f.awaitBlocked(t)
	w.cancel()
	require.NoError(t, tx.Rollback(t.Context()))
	assert.Equal(t, 1, w.stop(t), "the tick in flight at the stop completed")
	assert.Equal(t, 1, f.liveRows(t), "the writer's insert, not the rolled-back one, is the committed row")
}

// A statement held past its bound is a failure the writer records, so a
// stop is never left waiting behind a lock that is not going to be
// released; and a write that fails before any stop is recorded as the
// failure it is.
func TestWriterRecordsAStatementHeldPastItsBoundAndAFailedWrite(t *testing.T) {
	const seeded = 50
	f := newReconcileFixture(t, seeded)

	release := f.holdLock(t)
	w := f.startWriter(t, seeded, 5*time.Millisecond)
	f.awaitBlocked(t)
	select {
	case <-w.done:
	case <-time.After(3 * writerStatementBound):
		t.Fatal("the writer waited on the held lock past its statement bound")
	}
	require.ErrorIs(t, w.err, context.DeadlineExceeded)
	assert.ErrorContains(t, w.err, "live update")
	release()
	assert.Zero(t, f.liveRows(t), "the tick that ran out of time committed nothing")

	f.exec(t, `INSERT INTO %s (uniq, amount, label, blob) VALUES (100001, 1, 'clash', 'x')`)
	w = f.startWriter(t, seeded, 5*time.Millisecond)
	w.wg.Wait()
	require.Error(t, w.err)
	assert.ErrorContains(t, w.err, "live insert")
	assert.ErrorContains(t, w.err, "duplicate key")
}

// A reconcile under a steady write stream succeeds: the writer keeps
// updating rows in every chunk and inserting new ones while the recopy pass
// runs, so chunks the pass has recopied are written again before it ends.
// Under a reread policy that would be a repair error; under recopy the pass
// returns its repairs and the stream reopened at the new slot's consistent
// point carries every write that landed after it, and the shadow converges
// and verifies clean once the writer stops.
func TestReconcileUnderASteadyInsertStream(t *testing.T) {
	const seeded = 1500
	f := newReconcileFixture(t, seeded)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCopying, copier.Watermark{}, slot.Name(), slot.ConsistentPoint())))
	require.NoError(t, slot.Close(t.Context()))

	c := newCopier(t, f, shadow)
	first := f.startCapture(t, shadow, c, slot.ConsistentPoint())
	copyAll(t, f, c)
	status := first.awaitConfirmedPast(t, f.currentWALLSN(t))
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCatchingUp, c.Position().Watermark, slot.Name(), status.Confirmed)))

	// The failure, and a gap the pass has to repair.
	f.killWalsender(t, first, slot.Name())
	require.NoError(t, first.stream.Close(t.Context()))
	_, err = f.pool.Exec(t.Context(), `SELECT pg_catalog.pg_drop_replication_slot($1)`, slot.Name())
	require.NoError(t, err)
	f.exec(t, `UPDATE %s SET amount = 0 WHERE id BETWEEN 301 AND 310`)

	// The application keeps writing through the reconcile.
	writer := f.startWriter(t, seeded, 10*time.Millisecond)

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), f.pool, f.load(t, shadow))
	require.NoError(t, err, "a write landing in a recopied chunk is the stream's to settle, not the pass's to refuse")
	t.Cleanup(func() { assert.NoError(t, reconciled.Slot.Close(context.WithoutCancel(t.Context()))) })
	assert.NotEmpty(t, reconciled.Outcome.Repairs, "the gap and the live writes left chunks behind")
	assert.Equal(t, checkpoint.PhaseCatchingUp, reconciled.Checkpoint.Phase)

	// Writes that landed during the pass are behind the new slot's
	// consistent point whether or not the pass recopied them: the reopened
	// stream carries them, and only then does the shadow converge.
	second := f.startCapture(t, shadow, c, reconciled.Slot.ConsistentPoint())
	writes := writer.stop(t)
	require.Positive(t, writes, "the writer ran while the reconcile did")
	second.awaitConfirmedPast(t, f.currentWALLSN(t))
	second.stop(t)
	f.assertConverged(t, shadow)

	verifier, err := checksum.NewVerifier(f.target, shadow, f.lock, checksum.Options{})
	require.NoError(t, err)
	report, err := verifier.Verify(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean())
}
