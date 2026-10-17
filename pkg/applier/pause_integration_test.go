package applier_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
)

// The tests below hold the catch-up's flushes with Pause the way a live
// repair does while it recopies the shadow, and check what the catch-up
// does under the hold: it keeps reading and confirming the stream, buffers
// what the source writes, flushes none of it until the resume, and a Pause
// asked for while a flush is mid-write returns only once that flush is
// over.

// Under a Pause a source write reaches the buffer and is confirmed no
// further than its own position (ST-4), but the shadow is not written; a
// second Pause waits for the first's resume; the resume flushes what the
// hold buffered and the catch-up confirms past the write.
func TestCatchupPauseHoldsFlushesUntilResumed(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{positions: &switchPosition{pos: allLanded()}})

	resume, err := run.catchup.Pause(t.Context())
	require.NoError(t, err)
	t.Cleanup(resume)

	_, err = f.pool.Exec(t.Context(), `UPDATE `+f.table.Qualified()+` SET amount = amount + 1 WHERE id = 1`)
	require.NoError(t, err)
	end := f.currentWALLSN(t)
	run.awaitStatus(t, "buffer the write and read the stream past it", func(s applier.Status) bool {
		return s.Buffered == 1 && s.Delivered >= end
	})
	held := run.status(t)
	assert.True(t, held.FlushesHeld, "the status reports the hold")
	assert.Zero(t, held.Applied, "nothing is flushed while the hold lasts")
	assert.Less(t, held.Confirmed, end, "the buffered write keeps the confirmation below its position")

	const secondPauseWait = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), secondPauseWait)
	defer cancel()
	_, err = run.catchup.Pause(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "a second Pause waits for the first's resume")

	resume()
	assert.False(t, run.status(t).FlushesHeld, "the resume ends the hold the status reports")
	run.awaitStatus(t, "flush the buffered write and confirm past it", func(s applier.Status) bool {
		return s.Applied == 1 && s.Buffered == 0 && s.Confirmed >= end
	})
	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// A Pause asked for while a flush is mid-write returns once that flush is
// over, not before: a transaction holds SHARE MODE on the shadow so the
// flush waits for its ROW EXCLUSIVE lock, the Pause is seen not to return
// while the flush waits, and it returns once the transaction lets the
// flush commit. The hold is released well inside the flusher's lock wait,
// so the flush commits rather than failing.
func TestCatchupPauseWaitsForAFlushInFlight(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{positions: &switchPosition{pos: allLanded()}})
	shadow := pgx.Identifier{f.table.Schema, run.shadow.ShadowTable()}.Sanitize()

	hold, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Redundant safety closer: the test commits the hold itself.
		_ = hold.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = hold.Exec(t.Context(), `LOCK TABLE `+shadow+` IN SHARE MODE`)
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), `UPDATE `+f.table.Qualified()+` SET amount = amount + 1 WHERE id = 1`)
	require.NoError(t, err)
	const flushWaitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var waiting bool
		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE mode = 'RowExclusiveLock' AND NOT granted AND relation = $1::regclass)`,
			shadow).Scan(&waiting))
		return waiting
	}, flushWaitDeadline, 20*time.Millisecond, "the flush asked for ROW EXCLUSIVE on the shadow and is waiting behind the hold")
	assert.False(t, run.catchup.Status().FlushesHeld, "a cycle's own flush holding the gate is not a hold")

	paused := make(chan error, 1)
	go func() {
		resume, err := run.catchup.Pause(t.Context())
		if err == nil {
			resume()
		}
		paused <- err
	}()
	const notYetWait = 200 * time.Millisecond
	select {
	case err := <-paused:
		t.Fatalf("Pause returned (%v) while the flush was still mid-write", err)
	case <-time.After(notYetWait):
	}

	require.NoError(t, hold.Commit(t.Context()))
	const pauseDeadline = 10 * time.Second
	select {
	case err := <-paused:
		require.NoError(t, err, "Pause returned once the flush committed")
	case <-time.After(pauseDeadline):
		t.Fatalf("Pause did not return within %s of the flush being let through", pauseDeadline)
	}
	end := f.currentWALLSN(t)
	run.awaitStatus(t, "apply the write and confirm past it", func(s applier.Status) bool {
		return s.Applied == 1 && s.Confirmed >= end
	})
	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// A paused cycle drains and flushes nothing, so what the last drain left
// deferred and what the flushes have applied stand across it: row 2's
// chunk is in flight, so its change waits while row 1's is applied; under
// the hold a write to row 3 is buffered and the cycles go on reading the
// stream past it, and the status still reports row 2 deferred and one
// image applied. Once the hold is resumed and row 2's chunk lands, both
// waiting rows are flushed.
func TestCatchupPauseKeepsTheLastDrainsCountsStanding(t *testing.T) {
	f := newCatchupFixture(t, 20)
	positions := &switchPosition{pos: oneInFlight(t, 2)}
	run := startCatchupWith(t, f, catchupSetup{positions: positions})
	q := f.table.Qualified()

	_, err := f.pool.Exec(t.Context(), `UPDATE `+q+` SET amount = amount + 1 WHERE id IN (1, 2)`)
	require.NoError(t, err)
	run.awaitStatus(t, "apply row 1 and defer row 2 behind its chunk", func(s applier.Status) bool {
		return s.Applied == 1 && s.Deferred == 1
	})

	resume, err := run.catchup.Pause(t.Context())
	require.NoError(t, err)
	t.Cleanup(resume)
	_, err = f.pool.Exec(t.Context(), `UPDATE `+q+` SET amount = amount + 1 WHERE id = 3`)
	require.NoError(t, err)
	end := f.currentWALLSN(t)
	run.awaitStatus(t, "buffer row 3 under the hold and read the stream past it", func(s applier.Status) bool {
		return s.Buffered == 2 && s.Delivered >= end
	})
	held := run.status(t)
	assert.True(t, held.FlushesHeld)
	assert.Equal(t, 1, held.Deferred, "row 2 is still the last drain's one deferred entry")
	assert.Equal(t, uint64(1), held.Applied, "the paused cycles applied nothing")

	resume()
	positions.set(allLanded())
	run.awaitStatus(t, "flush rows 2 and 3 and confirm past them", func(s applier.Status) bool {
		return s.Applied == 3 && s.Buffered == 0 && s.Deferred == 0 && s.Confirmed >= end
	})
	run.stop(t)
	f.assertConverged(t, run.shadow)
}
