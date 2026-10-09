package applier_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
)

// A transaction that has written much but not committed pins the slot: the
// server cannot move the slot's restart position past the transaction's
// first record until it ends, however much the catch-up confirms, so the
// WAL the slot retains grows with every byte the transaction writes. Once
// that passes the ceiling the catch-up ends with the typed abort rather than
// keep a slot that could fill the volume (ST-3). The lock is intact and the
// slot is not lost: the abort is the ceiling's alone.
func TestCatchupEndsWhenTheSlotRetainsMoreThanTheCeiling(t *testing.T) {
	const ceiling = int64(1 << 20)
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{SlotLagCeiling: ceiling},
	})
	run.awaitStatus(t, "complete a cycle under the ceiling", func(s applier.Status) bool {
		return s.WALEnd > 0
	})

	// Several MiB of rows, written and left uncommitted for the rest of
	// the test: pgoutput sends nothing for them, and the slot keeps them
	// all. The catch-up's cycles run alongside the insert, so the abort
	// may land before the last row is written; the ceiling is crossed
	// either way.
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

	err = run.wait(t)
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, ceiling, exceeded.Ceiling)
	assert.Greater(t, exceeded.Retained, ceiling, "the abort carries the measure that crossed the ceiling")
	assert.NotErrorIs(t, err, decode.ErrSlotLost, "a slot over the ceiling is not a lost slot")
	assert.NotErrorIs(t, err, applier.ErrTableLockLost)
	assert.NoError(t, run.lock.Err(), "the abort is the ceiling's, not the lock's")

	status, found, inspectErr := decode.InspectSlot(t.Context(), f.pool, exceeded.Slot)
	require.NoError(t, inspectErr)
	require.True(t, found, "the abort leaves the slot in place for the caller to decide on")
	assert.False(t, status.Lost())
}
