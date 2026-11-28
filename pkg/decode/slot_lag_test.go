package decode_test

import (
	"math"
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
	over := reading(ceiling*2, ceiling/2)
	require.NoError(t, watch.Judge(over, lagTestStart), "the first reading over starts the grace")
	require.NoError(t, watch.Judge(over, lagTestStart.Add(lagTestGrace-time.Nanosecond)), "still within the grace")

	err := watch.Judge(over, lagTestStart.Add(lagTestGrace))
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagWALRetained,
		Retained: ceiling * 2, Unconfirmed: ceiling / 2, Ceiling: ceiling}, exceeded)
}

// The grace has a hard edge in bytes: retained WAL at exactly three times the
// ceiling is let stand while the grace runs, and one byte past it is the
// abort on that reading, with its own cause, well before the grace would
// have run out.
func TestSlotLagWatchAbortsAtOnceWhenRetainedWALPassesTheHardLimitWithinTheGrace(t *testing.T) {
	const ceiling = int64(4096)
	hardLimit := decode.RetainedWALHardMultiple * ceiling
	require.Equal(t, int64(12288), hardLimit, "the hard limit is three times the ceiling")
	watch := newLagWatch(t, ceiling, lagTestGrace)
	require.NoError(t, watch.Judge(reading(ceiling+1, 0), lagTestStart), "the first reading over starts the grace")
	require.NoError(t, watch.Judge(reading(hardLimit, ceiling), lagTestStart.Add(time.Second)), "at the hard limit the grace still stands")

	err := watch.Judge(reading(hardLimit+1, ceiling), lagTestStart.Add(2*time.Second))
	require.ErrorIs(t, err, decode.ErrSlotLagCeiling)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagWALRetainedHardLimit,
		Retained: hardLimit + 1, Unconfirmed: ceiling, Ceiling: ceiling}, exceeded)
}

// The hard limit applies from the first reading over the ceiling, before
// any grace has passed, and does not change the cause once the grace has
// run out: then the slot has stayed over the ceiling for the grace, which
// is the abort it names whatever the amount.
func TestSlotLagWatchHardLimitOnTheFirstReadingAndAfterTheGrace(t *testing.T) {
	const ceiling = int64(4096)
	hardLimit := decode.RetainedWALHardMultiple * ceiling
	var exceeded *decode.SlotLagExceededError

	first := newLagWatch(t, ceiling, lagTestGrace)
	require.ErrorAs(t, first.Judge(reading(hardLimit+1, 0), lagTestStart), &exceeded)
	assert.Equal(t, decode.SlotLagWALRetainedHardLimit, exceeded.Cause)

	outlasted := newLagWatch(t, ceiling, lagTestGrace)
	require.NoError(t, outlasted.Judge(reading(ceiling+1, 0), lagTestStart))
	require.ErrorAs(t, outlasted.Judge(reading(hardLimit+1, 0), lagTestStart.Add(lagTestGrace)), &exceeded)
	assert.Equal(t, decode.SlotLagWALRetained, exceeded.Cause)
}

// A ceiling so large that three times it would overflow holds the hard
// limit at the largest byte count rather than wrap it and abort every
// reading over the ceiling.
func TestSlotLagWatchHardLimitDoesNotOverflow(t *testing.T) {
	const ceiling = int64(math.MaxInt64/3 + 1)
	watch := newLagWatch(t, ceiling, lagTestGrace)
	require.NoError(t, watch.Judge(reading(math.MaxInt64, 0), lagTestStart))
}

// The hard limit a caller sizes a volume by is the one the watch judges
// against: three times the ceiling, held at the largest byte count where
// the product would overflow.
func TestRetainedHardLimitIsThreeCeilingsWithoutOverflow(t *testing.T) {
	assert.Equal(t, int64(3<<30), decode.RetainedHardLimit(1<<30))
	assert.Equal(t, int64(math.MaxInt64), decode.RetainedHardLimit(math.MaxInt64/3+1))
}

// Both retained-WAL causes are one family for a caller acting on the stop,
// and a stream that fell behind, or a cause this engine does not know, is
// not in it.
func TestSlotLagCauseRetainsWALNamesTheRetainedFamily(t *testing.T) {
	assert.True(t, decode.SlotLagWALRetained.RetainsWAL())
	assert.True(t, decode.SlotLagWALRetainedHardLimit.RetainsWAL())
	assert.False(t, decode.SlotLagStreamBehind.RetainsWAL())
	assert.False(t, decode.SlotLagCause("future-cause").RetainsWAL())
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
// binary units with the exact bytes beside them, what can cause it, and
// what lets the run resume. A whole number of a unit renders without a
// decimal. A cause the renderer does not know is named as it is, with no
// account borrowed from another cause.
func TestSlotLagExceededErrorRendersSizesForOperators(t *testing.T) {
	behind := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagStreamBehind,
		Retained: 1395864371, Unconfirmed: 1288490188, Ceiling: 1 << 30}
	assert.Equal(t,
		"slot pgsprite_0123abcd has 1.2 GiB of WAL its stream has not confirmed, over the 1 GiB ceiling (1288490188 > 1073741824 bytes); "+
			"the stream has fallen behind the source's writes, and the slot keeps that WAL unconfirmed after the stop, "+
			"so a resume under the same SlotLagCeiling is refused before it starts: "+
			"once the write rate has dropped, resume with a SlotLagCeiling with margin above the WAL the slot holds unconfirmed by then, "+
			"which grows past this figure while the run is stopped, and room on the volume for it, "+
			"or start the schema change over, which drops the slot and the WAL it holds",
		behind.Error())

	retained := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagWALRetained,
		Retained: 1536, Unconfirmed: 512, Ceiling: 1000}
	assert.Equal(t,
		"slot pgsprite_0123abcd retains 1.5 KiB of WAL, over the 1000 B ceiling (1536 > 1000 bytes), for longer than the server takes to release WAL the stream confirmed, "+
			"and the stream itself is 512 B behind; either a transaction on the source holds the WAL back (an open write transaction or a prepared one), "+
			"and the run can resume once it ends, or the cluster writes more WAL per release interval and catch-up cycle than the ceiling holds, "+
			"and the run needs a higher SlotLagCeiling, with margin above that much WAL",
		retained.Error())

	hard := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagWALRetainedHardLimit,
		Retained: 3073, Unconfirmed: 512, Ceiling: 1024}
	assert.Equal(t,
		"slot pgsprite_0123abcd retains 3.0 KiB of WAL, past the 3 KiB hard limit of 3 times the 1 KiB ceiling (3073 > 3072 bytes), "+
			"before the server's time to release WAL the stream confirmed ran out, and the stream itself is 512 B behind; "+
			"more than a healthy slot holds just before a release, which is about twice the ceiling; either a transaction on the source holds the WAL back "+
			"(an open write transaction or a prepared one), and the run can resume once it ends, or the source wrote WAL faster than the server releases it, "+
			"and the run needs a higher SlotLagCeiling or a lower write rate on the source",
		hard.Error())

	unknown := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: "future-cause",
		Retained: 1536, Unconfirmed: 512, Ceiling: 1000}
	assert.Equal(t, `slot pgsprite_0123abcd is over the 1000 B ceiling (cause "future-cause"): retains 1.5 KiB, 512 B unconfirmed`,
		unknown.Error())

	// A run of deadlocks in progress at the stop is named after the
	// measure, so the operator sees both causes; the zero CatchupDeadlocks
	// above adds nothing.
	backingOff := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: "future-cause",
		Retained: 1536, Unconfirmed: 512, Ceiling: 1000,
		Deadlocks: decode.CatchupDeadlocks{Consecutive: 6, BackingOff: true}}
	assert.Equal(t, `slot pgsprite_0123abcd is over the 1000 B ceiling (cause "future-cause"): retains 1.5 KiB, 512 B unconfirmed`+
		"; meanwhile the server had rolled the catch-up's flush back as a deadlock victim (6 in a row) and its batch was waiting out the backoff before the next retry, "+
		"because the copy and the catch-up were writing the same unique values into the shadow at once: "+
		"the rolled-back flushes left the shadow unchanged, and that churn must ease as well, beside the remedy for the measure, before a resume gets through",
		backingOff.Error())

	retrying := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: "future-cause",
		Retained: 1536, Unconfirmed: 512, Ceiling: 1000,
		Deadlocks: decode.CatchupDeadlocks{Consecutive: 1}}
	assert.Equal(t, `slot pgsprite_0123abcd is over the 1000 B ceiling (cause "future-cause"): retains 1.5 KiB, 512 B unconfirmed`+
		"; meanwhile the server had rolled the catch-up's flush back as a deadlock victim (1 in a row), "+
		"because the copy and the catch-up were writing the same unique values into the shadow at once: "+
		"the rolled-back flushes left the shadow unchanged, and that churn must ease as well, beside the remedy for the measure, before a resume gets through",
		retrying.Error())
}

// A stream-behind stop during a run of deadlocks names them after the
// measure: the batch waiting out its backoff is what kept the WAL
// unconfirmed, so the churn easing is part of the remedy beside the
// cause's own.
func TestStreamBehindStopNamesTheDeadlocksInProgress(t *testing.T) {
	behind := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: decode.SlotLagStreamBehind,
		Retained: 1536, Unconfirmed: 1200, Ceiling: 1000,
		Deadlocks: decode.CatchupDeadlocks{Consecutive: 6, BackingOff: true}}
	assert.Equal(t, "slot pgsprite_0123abcd has 1.2 KiB of WAL its stream has not confirmed, over the 1000 B ceiling (1200 > 1000 bytes); "+
		"the stream has fallen behind the source's writes, and the slot keeps that WAL unconfirmed after the stop, "+
		"so a resume under the same SlotLagCeiling is refused before it starts: "+
		"once the write rate has dropped, resume with a SlotLagCeiling with margin above the WAL the slot holds unconfirmed by then, "+
		"which grows past this figure while the run is stopped, and room on the volume for it, "+
		"or start the schema change over, which drops the slot and the WAL it holds"+
		"; meanwhile the server had rolled the catch-up's flush back as a deadlock victim (6 in a row) and its batch was waiting out the backoff before the next retry, "+
		"because the copy and the catch-up were writing the same unique values into the shadow at once: "+
		"the rolled-back flushes left the shadow unchanged, and that churn must ease as well, beside the remedy for the measure, before a resume gets through",
		behind.Error())
}

// A retained-WAL stop renders only its own cause, deadlocks or not: the
// stream was within the ceiling, so a resume once the deadlocks ease would
// stop again, and the cause's remedy (end the open transaction, or raise
// the ceiling) is the one that changes the outcome. The field still
// carries the deadlocks for a host.
func TestRetainedWALStopLeavesTheDeadlocksOutOfItsRemedy(t *testing.T) {
	deadlocks := decode.CatchupDeadlocks{Consecutive: 2, BackingOff: true}
	for _, cause := range []decode.SlotLagCause{decode.SlotLagWALRetained, decode.SlotLagWALRetainedHardLimit} {
		t.Run(string(cause), func(t *testing.T) {
			quiet := &decode.SlotLagExceededError{Slot: lagTestSlot, Cause: cause,
				Retained: 3073, Unconfirmed: 512, Ceiling: 1024}
			during := *quiet
			during.Deadlocks = deadlocks
			assert.Equal(t, quiet.Error(), during.Error())
		})
	}
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
