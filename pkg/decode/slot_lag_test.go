package decode_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

const lagTestSlot = "pgsprite_0123abcd"

// A slot at the ceiling is within it; one byte over is the typed abort,
// carrying the figures an operator compares.
func TestWithinLagCeilingAbortsOnlyAboveTheCeiling(t *testing.T) {
	const ceiling = int64(4096)
	at := decode.SlotStatus{Name: lagTestSlot, WALStatus: decode.WALStatusExtended,
		Retained: decode.RetainedWAL{Bytes: ceiling, Known: true}}
	require.NoError(t, at.WithinLagCeiling(ceiling), "retaining exactly the ceiling is within it")

	over := at
	over.Retained.Bytes = ceiling + 1
	err := over.WithinLagCeiling(ceiling)
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, &decode.SlotLagExceededError{Slot: lagTestSlot, Retained: ceiling + 1, Ceiling: ceiling}, exceeded)
	assert.NotErrorIs(t, err, decode.ErrSlotLost, "over the ceiling is not loss")
}

// The server's verdict outranks any measure: a lost slot is the lost state
// even when it still reports a small retained figure, and it is never the
// ceiling abort.
func TestWithinLagCeilingReportsALostSlotBeforeMeasuring(t *testing.T) {
	lost := decode.SlotStatus{Name: lagTestSlot, WALStatus: decode.WALStatusLost,
		Retained: decode.RetainedWAL{Bytes: 1, Known: true}}
	assert.True(t, lost.Lost())
	err := lost.WithinLagCeiling(decode.DefaultSlotLagCeiling)
	require.ErrorIs(t, err, decode.ErrSlotLost)
	var state *decode.SlotLostError
	require.ErrorAs(t, err, &state)
	assert.Equal(t, &decode.SlotLostError{Slot: lagTestSlot, Found: true, WALStatus: decode.WALStatusLost}, state)
	assert.NotErrorIs(t, err, decode.ErrSlotLagCeiling)
}

// A retained amount the server could not measure is never read as nothing
// retained: it fails the check on its own sentinel, distinct from loss and
// from the ceiling.
func TestWithinLagCeilingFailsClosedOnAnUnknownMeasure(t *testing.T) {
	unknown := decode.SlotStatus{Name: lagTestSlot, WALStatus: decode.WALStatusReserved}
	assert.False(t, unknown.Lost())
	err := unknown.WithinLagCeiling(decode.DefaultSlotLagCeiling)
	require.ErrorIs(t, err, decode.ErrSlotLagUnknown)
	assert.NotErrorIs(t, err, decode.ErrSlotLost)
	assert.NotErrorIs(t, err, decode.ErrSlotLagCeiling)
}

// A ceiling that would abort every slot is a programming error, refused
// before the slot is judged.
func TestWithinLagCeilingRefusesACeilingBelowOneByte(t *testing.T) {
	within := decode.SlotStatus{Name: lagTestSlot, Retained: decode.RetainedWAL{Bytes: 0, Known: true}}
	require.ErrorIs(t, within.WithinLagCeiling(0), decode.ErrInvariantViolation)
	require.NoError(t, within.WithinLagCeiling(1))
}

// The ceiling abort reads as an operator message: sizes in binary units
// with the exact bytes beside them, the usual cause, and what lets the run
// resume. A whole number of a unit renders without a decimal.
func TestSlotLagExceededErrorRendersSizesForOperators(t *testing.T) {
	exceeded := &decode.SlotLagExceededError{Slot: lagTestSlot, Retained: 1288490188, Ceiling: 1 << 30}
	assert.Equal(t,
		"slot pgsprite_0123abcd retains 1.2 GiB of WAL, over the 1 GiB ceiling (1288490188 > 1073741824 bytes); "+
			"the usual cause is a long-open transaction on the source, and the run can resume once it ends",
		exceeded.Error())

	small := &decode.SlotLagExceededError{Slot: lagTestSlot, Retained: 1536, Ceiling: 1000}
	assert.Equal(t,
		"slot pgsprite_0123abcd retains 1.5 KiB of WAL, over the 1000 B ceiling (1536 > 1000 bytes); "+
			"the usual cause is a long-open transaction on the source, and the run can resume once it ends",
		small.Error())
}

// A slot that is gone is the lost state too, worded as absence.
func TestSlotLostErrorDistinguishesAnAbsentSlot(t *testing.T) {
	gone := &decode.SlotLostError{Slot: lagTestSlot}
	assert.ErrorIs(t, gone, decode.ErrSlotLost)
	assert.Equal(t, "slot pgsprite_0123abcd no longer exists", gone.Error())
	lost := &decode.SlotLostError{Slot: lagTestSlot, Found: true, WALStatus: decode.WALStatusLost}
	assert.Equal(t, `slot pgsprite_0123abcd is lost: the server reports wal_status "lost" (cause not reported by this server version)`, lost.Error())
}

// The server's cause rides on the lost state: its word when it has one,
// and otherwise whether it has no cause column or reported no cause.
func TestSlotLostErrorRendersTheServersCause(t *testing.T) {
	removed := &decode.SlotLostError{Slot: lagTestSlot, Found: true, WALStatus: decode.WALStatusLost,
		Invalidation: decode.SlotInvalidation{Reason: decode.InvalidationWALRemoved, Reported: true}}
	assert.Equal(t, `slot pgsprite_0123abcd is lost: the server reports wal_status "lost" (wal_removed)`, removed.Error())
	silent := &decode.SlotLostError{Slot: lagTestSlot, Found: true, WALStatus: decode.WALStatusLost,
		Invalidation: decode.SlotInvalidation{Reported: true}}
	assert.Equal(t, `slot pgsprite_0123abcd is lost: the server reports wal_status "lost" (no invalidation reported)`, silent.Error())
}

// LostState is the lost state only for a slot the server reports lost,
// carrying the cause the status read; a readable slot has none.
func TestLostStateCarriesTheCause(t *testing.T) {
	lost := decode.SlotStatus{Name: lagTestSlot, WALStatus: decode.WALStatusLost,
		Invalidation: decode.SlotInvalidation{Reason: decode.InvalidationWALRemoved, Reported: true}}
	assert.Equal(t, &decode.SlotLostError{Slot: lagTestSlot, Found: true, WALStatus: decode.WALStatusLost,
		Invalidation: lost.Invalidation}, lost.LostState())
	assert.Nil(t, decode.SlotStatus{Name: lagTestSlot, WALStatus: decode.WALStatusReserved}.LostState())
}
