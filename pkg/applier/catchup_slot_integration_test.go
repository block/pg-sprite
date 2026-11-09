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

// awaitCycleAfter writes one committed row and waits for a cycle to record
// a WAL end at or past it: every cycle that began before the write has by
// then judged the slot.
func (r *catchupRun) awaitCycleAfter(t *testing.T, what string) {
	t.Helper()
	_, err := r.f.pool.Exec(t.Context(), `UPDATE `+r.f.table.Qualified()+` SET amount = amount + 1 WHERE id = 1`)
	require.NoError(t, err)
	tick := r.f.currentWALLSN(t)
	r.awaitStatus(t, what, func(s applier.Status) bool { return s.WALEnd >= tick })
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

	// Several MiB of rows, written and left uncommitted for the rest of
	// the test: pgoutput sends nothing for them, and the slot keeps them
	// all.
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Logf("roll back the pinning transaction: %v", err)
		}
	})
	_, err = tx.Exec(t.Context(), `
		INSERT INTO `+f.table.Qualified()+` (uniq, amount, label, blob)
		SELECT -g, 0, 'pinned', repeat('w', 1024)
		FROM generate_series(1, 4096) AS g`)
	require.NoError(t, err)

	const overDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		status, found, err := decode.InspectSlot(t.Context(), f.pool, run.slot)
		return err == nil && found && status.Retained.Known && status.Retained.Bytes > slotLagTestCeiling
	}, overDeadline, 20*time.Millisecond, "the pinned rows should take the retained WAL over the ceiling")
	run.awaitCycleAfter(t, "run a cycle that could see the slot over the ceiling")
	run.awaitCycleAfter(t, "run a cycle after the one that judged the slot over the ceiling")
	run.status(t) // still running: retained WAL over the ceiling is let stand for the grace

	clock.advance(decode.RestartReleaseInterval + applier.DefaultCatchupInterval)
	err = run.wait(t)
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

	// Several MiB of committed rows: the catch-up buffers them under the
	// hold and cannot confirm past their commit until they are flushed.
	_, err = f.pool.Exec(t.Context(), `
		INSERT INTO `+f.table.Qualified()+` (uniq, amount, label, blob)
		SELECT -g, 0, 'held', repeat('w', 1024)
		FROM generate_series(1, 4096) AS g`)
	require.NoError(t, err)

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
