package decode_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

const lagTestSlot = "pgsprite_0123abcd"

// lagTestGrace is how long a watch under test lets retained WAL stand
// over the ceiling; the readings are judged at times a test clock chooses.
const lagTestGrace = 20 * time.Second

var lagTestStart = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// reading is a readable slot whose consumer has yet to confirm
// unconfirmed bytes of the retained bytes it keeps.
func reading(retained, unconfirmed int64) decode.SlotStatus {
	return decode.SlotStatus{Name: lagTestSlot, WALStatus: decode.WALStatusExtended,
		Retained:    decode.RetainedWAL{Bytes: retained, Known: true},
		Unconfirmed: decode.RetainedWAL{Bytes: unconfirmed, Known: true}}
}

func newLagWatch(t *testing.T, ceiling int64, grace time.Duration) *decode.SlotLagWatch {
	t.Helper()
	watch, err := decode.NewSlotLagWatch(ceiling, grace)
	require.NoError(t, err)
	return watch
}

// WAL the stream has yet to confirm is the stream's own lag: one reading
// over the ceiling is the typed abort at once, naming the stream as the
// cause and carrying both measures an operator compares. Exactly the
// ceiling is within it.
func TestSlotLagWatchAbortsAtOnceWhenTheStreamIsBehind(t *testing.T) {
	const ceiling = int64(4096)
	watch := newLagWatch(t, ceiling, lagTestGrace)
	require.NoError(t, watch.Judge(reading(ceiling, ceiling), lagTestStart), "exactly the ceiling is within it")

	err := watch.Judge(reading(ceiling+1, ceiling+1), lagTestStart)
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagStreamBehind,
		Retained: ceiling + 1, Unconfirmed: ceiling + 1, Ceiling: ceiling}, exceeded)
	assert.NotErrorIs(t, err, decode.ErrSlotLost, "over the ceiling is not loss")
}

// WAL the slot retains beyond what the stream has yet to confirm is WAL
// the server has not released yet: a reading over the ceiling is within it
// until the readings have stayed over for the grace, and the abort then
// names the retained WAL as the cause with both measures as last read.
func TestSlotLagWatchLetsRetainedWALStandOverTheCeilingForTheGrace(t *testing.T) {
	const ceiling = int64(4096)
	watch := newLagWatch(t, ceiling, lagTestGrace)
	over := reading(ceiling*3, ceiling/2)
	require.NoError(t, watch.Judge(over, lagTestStart), "the first reading over starts the grace")
	require.NoError(t, watch.Judge(over, lagTestStart.Add(lagTestGrace-time.Nanosecond)), "still within the grace")

	err := watch.Judge(over, lagTestStart.Add(lagTestGrace))
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagWALRetained,
		Retained: ceiling * 3, Unconfirmed: ceiling / 2, Ceiling: ceiling}, exceeded)
}

// A reading within the ceiling ends the grace: the server released the
// WAL, so a later reading over it starts a new grace rather than inherit
// the old one.
func TestSlotLagWatchRestartsTheGraceAfterAReadingWithinTheCeiling(t *testing.T) {
	const ceiling = int64(4096)
	watch := newLagWatch(t, ceiling, lagTestGrace)
	over := reading(ceiling+1, 0)
	require.NoError(t, watch.Judge(over, lagTestStart))
	require.NoError(t, watch.Judge(reading(ceiling, 0), lagTestStart.Add(lagTestGrace/2)), "released: within the ceiling")
	require.NoError(t, watch.Judge(over, lagTestStart.Add(lagTestGrace)), "over again: a new grace begins")
	require.NoError(t, watch.Judge(over, lagTestStart.Add(2*lagTestGrace-time.Nanosecond)))
	require.ErrorIs(t, watch.Judge(over, lagTestStart.Add(2*lagTestGrace)), decode.ErrSlotLagCeiling)
}

// With no grace, retained WAL is judged on one reading as the unconfirmed
// WAL is.
func TestSlotLagWatchWithNoGraceAbortsOnTheFirstRetainedReadingOver(t *testing.T) {
	const ceiling = int64(4096)
	watch := newLagWatch(t, ceiling, 0)
	err := watch.Judge(reading(ceiling+1, 0), lagTestStart)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, decode.SlotLagWALRetained, exceeded.Cause)
}

// The server's verdict outranks any measure: a lost slot is the lost state
// even when it still reports a small retained figure, and it is never the
// ceiling abort.
func TestSlotLagWatchReportsALostSlotBeforeMeasuring(t *testing.T) {
	lost := decode.SlotStatus{Name: lagTestSlot, WALStatus: decode.WALStatusLost,
		Retained:    decode.RetainedWAL{Bytes: 1, Known: true},
		Unconfirmed: decode.RetainedWAL{Bytes: 1, Known: true}}
	assert.True(t, lost.Lost())
	err := newLagWatch(t, decode.DefaultSlotLagCeiling, lagTestGrace).Judge(lost, lagTestStart)
	require.ErrorIs(t, err, decode.ErrSlotLost)
	var state *decode.SlotLostError
	require.ErrorAs(t, err, &state)
	assert.Equal(t, &decode.SlotLostError{Slot: lagTestSlot, Found: true, WALStatus: decode.WALStatusLost}, state)
	assert.NotErrorIs(t, err, decode.ErrSlotLagCeiling)
}

// An amount the server could not measure is never read as nothing: either
// measure unknown fails the check on its own sentinel, distinct from loss
// and from the ceiling.
func TestSlotLagWatchFailsClosedOnAnUnknownMeasure(t *testing.T) {
	watch := newLagWatch(t, decode.DefaultSlotLagCeiling, lagTestGrace)
	neither := decode.SlotStatus{Name: lagTestSlot, WALStatus: decode.WALStatusReserved}
	assert.False(t, neither.Lost())
	retainedOnly := neither
	retainedOnly.Retained = decode.RetainedWAL{Bytes: 1, Known: true}
	unconfirmedOnly := neither
	unconfirmedOnly.Unconfirmed = decode.RetainedWAL{Bytes: 1, Known: true}
	for _, unknown := range []decode.SlotStatus{neither, retainedOnly, unconfirmedOnly} {
		err := watch.Judge(unknown, lagTestStart)
		require.ErrorIs(t, err, decode.ErrSlotLagUnknown)
		assert.NotErrorIs(t, err, decode.ErrSlotLost)
		assert.NotErrorIs(t, err, decode.ErrSlotLagCeiling)
	}
}

// A ceiling that would abort every slot, or a grace that runs backwards,
// is a programming error, refused before any slot is judged.
func TestNewSlotLagWatchRefusesABadCeilingOrGrace(t *testing.T) {
	_, err := decode.NewSlotLagWatch(0, lagTestGrace)
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	_, err = decode.NewSlotLagWatch(1, -time.Nanosecond)
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	watch := newLagWatch(t, 1, 0)
	require.NoError(t, watch.Judge(reading(1, 1), lagTestStart))
}

// The ceiling abort reads as an operator message for each cause: sizes in
// binary units with the exact bytes beside them, the usual cause, and what
// lets the run resume. A whole number of a unit renders without a decimal.
func TestSlotLagExceededErrorRendersSizesForOperators(t *testing.T) {
	behind := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagStreamBehind,
		Retained: 1395864371, Unconfirmed: 1288490188, Ceiling: 1 << 30}
	assert.Equal(t,
		"slot pgsprite_0123abcd has 1.2 GiB of WAL its stream has not confirmed, over the 1 GiB ceiling (1288490188 > 1073741824 bytes); "+
			"the stream has fallen behind the source's writes, and running the schema change again resumes it from the slot once the write rate allows",
		behind.Error())

	retained := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagWALRetained,
		Retained: 1536, Unconfirmed: 512, Ceiling: 1000}
	assert.Equal(t,
		"slot pgsprite_0123abcd retains 1.5 KiB of WAL, over the 1000 B ceiling (1536 > 1000 bytes), for longer than the server takes to release WAL the stream confirmed, "+
			"and the stream itself is 512 B behind; the usual cause is a long-open transaction on the source, and the run can resume once it ends",
		retained.Error())
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
