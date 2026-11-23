package applier_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
)

// shiftedClock is the wall clock moved ahead by an offset a test grows,
// so a wait the catch-up measures by its clock passes without the test
// waiting it out.
type shiftedClock struct {
	mu    sync.Mutex
	ahead time.Duration
}

func (c *shiftedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.ahead)
}

func (c *shiftedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ahead += d
}

// slotLagTestCeiling is the ceiling the tests here set: small enough for a
// few MiB of rows to cross it.
const slotLagTestCeiling = int64(1 << 20)

// awaitTickPast writes one committed row and waits for the catch-up's
// status to reach its position by the caller's measure.
func (r *catchupRun) awaitTickPast(t *testing.T, what string, reached func(s applier.Status, tick decode.LSN) bool) {
	t.Helper()
	_, err := r.f.pool.Exec(t.Context(), `UPDATE `+r.f.table.Qualified()+` SET amount = amount + 1 WHERE id = 1`)
	require.NoError(t, err)
	tick := r.f.currentWALLSN(t)
	r.awaitStatus(t, what, func(s applier.Status) bool { return reached(s, tick) })
}

// awaitCycleAfter waits for a cycle to record a WAL end at or past a fresh
// committed row: every cycle that began before the write has by then
// judged the slot.
func (r *catchupRun) awaitCycleAfter(t *testing.T, what string) {
	t.Helper()
	r.awaitTickPast(t, what, func(s applier.Status, tick decode.LSN) bool { return s.WALEnd >= tick })
}

// A transaction that has written much but not committed pins the slot: the
// server cannot move the slot's restart position past the transaction's
// first record until it ends, however much the catch-up confirms, so the
// WAL the slot retains grows with every byte the transaction writes while
// the WAL the stream has yet to confirm stays small. Retained WAL over the
// ceiling is let stand while the server could still be releasing it; once
// it has stood for the grace the catch-up ends with the typed abort naming
// the retained WAL as the cause, rather than keep a slot that could fill
// the volume (ST-3). The lock is intact and the slot is not lost: the abort
// is the ceiling's alone. The grace passes on the catch-up's clock, which
// the test moves ahead once a cycle has judged the slot over the ceiling.
func TestCatchupEndsWhenTheSlotRetainsMoreThanTheCeiling(t *testing.T) {
	clock := &shiftedClock{}
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{SlotLagCeiling: slotLagTestCeiling, Clock: clock},
	})
	run.awaitStatus(t, "complete a cycle under the ceiling", func(s applier.Status) bool {
		return s.WALEnd > 0
	})

	// Rows written in small steps and left uncommitted for the rest of the
	// test until the slot retains just over the ceiling: pgoutput sends
	// nothing for them, and the slot keeps them all, short of the hard
	// limit that would end the run within the grace.
	pin := pinSlot(t, f)
	for slotRetained(t, f, run.slot) <= slotLagTestCeiling {
		require.False(t, pin.step(t, run), "the catch-up ended while the slot was within the ceiling")
	}
	require.LessOrEqual(t, slotRetained(t, f, run.slot), decode.RetainedWALHardMultiple*slotLagTestCeiling,
		"the pin stays within the hard limit, so only the grace can end the run")
	run.awaitCycleAfter(t, "run a cycle that could see the slot over the ceiling")
	run.awaitCycleAfter(t, "run a cycle after the one that judged the slot over the ceiling")
	run.status(t) // still running: retained WAL over the ceiling is let stand for the grace

	clock.advance(decode.RestartReleaseInterval / 2)
	run.awaitCycleAfter(t, "run a cycle half a release interval after the slot went over the ceiling")
	run.status(t) // half the server's release interval later the server may still release it

	clock.advance(decode.RestartReleaseInterval/2 + applier.DefaultCatchupInterval)
	err := run.wait(t)
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, decode.SlotLagWALRetained, exceeded.Cause, "the server kept the WAL; the stream had confirmed it")
	assert.Equal(t, slotLagTestCeiling, exceeded.Ceiling)
	assert.Greater(t, exceeded.Retained, slotLagTestCeiling, "the abort carries the measure that crossed the ceiling")
	assert.LessOrEqual(t, exceeded.Unconfirmed, slotLagTestCeiling, "the stream itself kept up")
	assert.NotErrorIs(t, err, decode.ErrSlotLost, "a slot over the ceiling is not a lost slot")
	assert.NotErrorIs(t, err, applier.ErrTableLockLost)
	assert.NoError(t, run.lock.Err(), "the abort is the ceiling's, not the lock's")

	status, found, inspectErr := decode.InspectSlot(t.Context(), f.pool, exceeded.Slot)
	require.NoError(t, inspectErr)
	require.True(t, found, "the abort leaves the slot in place for the caller to decide on")
	assert.False(t, status.Lost())
}

// slowClock runs at a tenth of the wall clock's pace from start.
type slowClock struct{ start time.Time }

func (c slowClock) Now() time.Time { return c.start.Add(time.Since(c.start) / 10) }

// The grace has a hard edge: a pinned slot whose retained WAL passes three
// times the ceiling ends the catch-up on the cycle that reads it, however
// much of the grace is left, since no release the server is about to make
// accounts for that much WAL (ST-3). The cause names the hard limit, apart
// from the grace running out. The catch-up's clock runs at a tenth of the
// wall clock's pace, with a cycle of a tenth of the default, so its cycles
// end as often as usual while its grace outlasts the test's own deadline:
// however slowly the steps run, only the hard limit can end the run in time.
func TestCatchupEndsAtOnceWhenTheSlotRetainsPastTheHardLimit(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts: applier.CatchupOptions{
			SlotLagCeiling: slotLagTestCeiling,
			Clock:          slowClock{start: time.Now()},
			Interval:       applier.DefaultCatchupInterval / 10,
		},
	})
	run.awaitStatus(t, "complete a cycle under the ceiling", func(s applier.Status) bool {
		return s.WALEnd > 0
	})

	// Rows written in small steps and left uncommitted until the catch-up
	// ends, which it does once the slot retains past three times the ceiling:
	// each step is small and waits for a cycle, so the stream keeps up and
	// the retained WAL is the only measure that grows.
	pin := pinSlot(t, f)
	const pinDeadline = 30 * time.Second
	deadline := time.Now().Add(pinDeadline)
	for !pin.step(t, run) {
		require.True(t, time.Now().Before(deadline), "the catch-up should end once the slot retains past the hard limit")
	}

	err := run.wait(t)
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, decode.SlotLagWALRetainedHardLimit, exceeded.Cause, "past the hard limit, the grace does not stand")
	assert.Greater(t, exceeded.Retained, decode.RetainedWALHardMultiple*slotLagTestCeiling)
	assert.LessOrEqual(t, exceeded.Unconfirmed, slotLagTestCeiling, "the stream itself kept up")
	assert.NoError(t, run.lock.Err(), "the abort is the ceiling's, not the lock's")
}

// slotPin is a transaction left open on the fixture's pool, rolled back
// when the test ends: rows it writes and leaves uncommitted hold the
// slot's restart position, so the slot retains every byte of them.
type slotPin struct {
	tx    pgx.Tx
	steps int
}

func pinSlot(t *testing.T, f catchupFixture) *slotPin {
	t.Helper()
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Logf("roll back the pinning transaction: %v", err)
		}
	})
	return &slotPin{tx: tx}
}

// pinStepRows is how many rows of about a KiB each one step writes: about
// a third of the ceiling in WAL, so a step never takes the slot from
// within the ceiling to past the hard limit, nor leaves the stream a
// ceiling behind, yet a few steps cross from the ceiling to the hard limit
// well inside the grace.
const pinStepRows = 256

// step writes one step of uncommitted rows, then commits a one-row write
// on the pool, which flushes the step's WAL so the server's write position
// counts it, and waits for a cycle to judge the slot at or past that
// write and for the stream to confirm past it. An open transaction's
// records are not decoded until it commits, but the walsender reads past
// them and reports its read position in keepalives, so the confirmation
// moves past them too: the WAL the stream has yet to confirm stays within
// a step while the pinned restart position keeps the retained WAL
// growing. It reports whether the catch-up ended instead.
func (p *slotPin) step(t *testing.T, r *catchupRun) bool {
	t.Helper()
	_, err := p.tx.Exec(t.Context(), `
		INSERT INTO `+r.f.table.Qualified()+` (uniq, amount, label, blob)
		SELECT -g, 0, 'pinned', repeat('w', 1024)
		FROM generate_series($1::int + 1, $1::int + $2::int) AS g`, p.steps*pinStepRows, pinStepRows)
	require.NoError(t, err)
	p.steps++
	_, err = r.f.pool.Exec(t.Context(), `UPDATE `+r.f.table.Qualified()+` SET amount = amount + 1 WHERE id = 1`)
	require.NoError(t, err)
	tick := r.f.currentWALLSN(t)
	const stepDeadline = 30 * time.Second
	deadline := time.NewTimer(stepDeadline)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case err := <-r.done:
			r.done <- err
			return true
		default:
		}
		if status := r.catchup.Status(); status.WALEnd >= tick && status.Confirmed >= tick {
			return false
		}
		select {
		case <-deadline.C:
			t.Fatalf("catch-up did not judge the slot past %s within %s", tick, stepDeadline)
		case <-poll.C:
		}
	}
}

// slotRetained is the WAL the slot keeps on the volume as the server
// reports it now.
func slotRetained(t *testing.T, f catchupFixture, slot string) int64 {
	t.Helper()
	status, found, err := decode.InspectSlot(t.Context(), f.pool, slot)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, status.Retained.Known)
	return status.Retained.Bytes
}

// Changes the catch-up has buffered but not flushed hold its confirmation
// below their commit, so the WAL the stream has yet to confirm grows with
// every committed write while the flushes are held. That lag is the
// stream's own, and the first cycle that reads it over the ceiling ends
// the catch-up with the typed abort naming the stream as the cause, with
// no grace: no release by the server would shrink it (ST-3).
func TestCatchupEndsAtOnceWhenTheStreamFallsBehindTheCeiling(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{SlotLagCeiling: slotLagTestCeiling},
	})
	run.awaitStatus(t, "complete a cycle under the ceiling", func(s applier.Status) bool {
		return s.WALEnd > 0
	})
	resume, err := run.catchup.Pause(t.Context())
	require.NoError(t, err)
	t.Cleanup(resume)

	// Several MiB of rows in four committed transactions: the catch-up
	// buffers them under the hold and cannot confirm past the first one's
	// commit until they are flushed. One transaction would not do: a
	// keepalive the server sends while it is still writing can move the
	// confirmable position to its middle, so the lag left behind its
	// commit could stay under the ceiling. The three after the first are
	// several times the ceiling whatever the keepalives say, which the
	// precondition after the loop holds.
	var afterFirst decode.LSN
	for batch := range 4 {
		if batch == 1 {
			afterFirst = f.currentWALLSN(t)
		}
		_, err = f.pool.Exec(t.Context(), `
			INSERT INTO `+f.table.Qualified()+` (uniq, amount, label, blob)
			SELECT -g, 0, 'held', repeat('w', 1024)
			FROM generate_series($1 * 1024 + 1, ($1 + 1) * 1024) AS g`, batch)
		require.NoError(t, err)
	}
	require.Greater(t, int64(f.currentWALLSN(t)-afterFirst), slotLagTestCeiling,
		"the transactions after the first must write more WAL than the ceiling, whatever a keepalive confirms")

	err = run.wait(t)
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, decode.SlotLagStreamBehind, exceeded.Cause, "the stream had not confirmed the WAL; the server could not release it")
	assert.Equal(t, slotLagTestCeiling, exceeded.Ceiling)
	assert.Greater(t, exceeded.Unconfirmed, slotLagTestCeiling, "the abort carries the measure that crossed the ceiling")
	assert.NotErrorIs(t, err, decode.ErrSlotLost, "a stream behind the ceiling is not a lost slot")
	assert.NotErrorIs(t, err, applier.ErrTableLockLost)
	assert.NoError(t, run.lock.Err(), "the abort is the ceiling's, not the lock's")

	status, found, inspectErr := decode.InspectSlot(t.Context(), f.pool, exceeded.Slot)
	require.NoError(t, inspectErr)
	require.True(t, found, "the abort leaves the slot in place for the caller to decide on")
	assert.False(t, status.Lost())
}
