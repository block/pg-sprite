package applier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/progress"
)

// The retained WAL's grace is the server's release interval plus one
// cycle, so a slower cycle lengthens it by exactly its own length.
func TestSlotLagGraceIsTheReleaseIntervalPlusOneCycle(t *testing.T) {
	assert.Equal(t, 15*time.Second+DefaultCatchupInterval, slotLagGrace(CatchupOptions{}.withDefaults()))
	assert.Equal(t, 17*time.Second, slotLagGrace(CatchupOptions{Interval: 2 * time.Second}))
}

// busySourceSnapshotInterval is how often the modelled server logs a
// running-transactions record: PostgreSQL's background writer interval,
// written out rather than read from decode.RestartReleaseInterval so the
// model stays the server's if the constant drifts.
const busySourceSnapshotInterval = 15 * time.Second

// busySourceRate is the modelled source's write rate, in WAL bytes a second.
const busySourceRate = int64(1 << 20)

// busySourceBytes is the WAL the modelled source writes in d.
func busySourceBytes(d time.Duration) int64 {
	return d.Microseconds() * busySourceRate / int64(time.Second/time.Microsecond)
}

// busySourceRun is what judging a busy source's slot came to: the watch's
// first error, and the most WAL the slot retained before it.
type busySourceRun struct {
	err         error
	maxRetained int64
}

// busySourceTiming is when the modelled server logs its records and how
// far the stream runs behind the write position.
type busySourceTiming struct {
	record    time.Duration
	streamLag time.Duration
}

// onTime is the server logging a record every busySourceSnapshotInterval
// with the stream one cycle behind.
func onTime(opts CatchupOptions) busySourceTiming {
	return busySourceTiming{record: busySourceSnapshotInterval, streamLag: opts.Interval}
}

// judgeBusySource judges, one reading a cycle for span, the slot of a
// source that writes at busySourceRate with a transaction in progress at
// every running-transactions record the server logs at timing.record, and
// a stream timing.streamLag behind it. With a transaction in progress, the server moves the
// restart position at a record only to the record before
// (SnapBuildProcessRunningXacts restarts from the oldest such
// transaction's last serialized snapshot), and only once the stream has
// confirmed past the record (LogicalConfirmReceivedLocation). The slot
// is created at time zero, where its restart position stands until the
// second record releases it to the first.
func judgeBusySource(t *testing.T, opts CatchupOptions, timing busySourceTiming, ceiling int64, span time.Duration) busySourceRun {
	t.Helper()
	watch, err := decode.NewSlotLagWatch(ceiling, slotLagGrace(opts))
	require.NoError(t, err)
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	var run busySourceRun
	for at := opts.Interval; at <= span; at += opts.Interval {
		write := busySourceBytes(at)
		confirmedAt := max(at-timing.streamLag, 0)
		confirmed := busySourceBytes(confirmedAt)
		confirmedRecords := int64(confirmedAt / timing.record)
		restart := busySourceBytes(time.Duration(max(confirmedRecords-1, 0)) * timing.record)
		retained := write - restart
		run.err = watch.Judge(decode.SlotStatus{
			Name:        "pgsprite_0123abcd",
			WALStatus:   decode.WALStatusReserved,
			Retained:    decode.RetainedWAL{Bytes: retained, Known: true},
			Unconfirmed: decode.RetainedWAL{Bytes: write - confirmed, Known: true},
		}, start.Add(at))
		if run.err != nil {
			return run
		}
		run.maxRetained = max(run.maxRetained, retained)
	}
	return run
}

// A busy source's slot retains up to two of the server's release
// intervals of writes while its stream keeps up, since each release
// moves the restart position only to the record before. A ceiling that
// holds one grace of the source's writes is the least that lets the
// release that follows bring the retained WAL back within it, and the
// grace then outlasts every stretch over the ceiling, release after
// release.
func TestSlotLagGraceOutlastsABusySourceWhoseCeilingHoldsOneGraceOfWrites(t *testing.T) {
	opts := CatchupOptions{}.withDefaults()
	run := judgeBusySource(t, opts, onTime(opts), busySourceBytes(slotLagGrace(opts)), 10*busySourceSnapshotInterval)
	require.NoError(t, run.err)
	assert.Equal(t, busySourceBytes(2*busySourceSnapshotInterval), run.maxRetained,
		"the slot retains two release intervals of writes just before each release, well over the ceiling")
}

// The least ceiling holds only on time: a record the server logs a little
// late, or a stream a cycle slower, keeps the slot over it past the grace.
// Ten percent above one grace of writes survives both together, which is
// the margin the guide asks for.
func TestSlotLagGraceNeedsMarginAboveTheLeastCeiling(t *testing.T) {
	opts := CatchupOptions{}.withDefaults()
	least := busySourceBytes(slotLagGrace(opts))
	late := busySourceTiming{record: busySourceSnapshotInterval + 200*time.Millisecond, streamLag: 2 * opts.Interval}
	span := 10 * busySourceSnapshotInterval

	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, judgeBusySource(t, opts, late, least, span).err, &exceeded,
		"the least ceiling is crossed for good once a record is late and the stream a cycle slower")
	assert.Equal(t, decode.SlotLagWALRetained, exceeded.Cause)
	require.NoError(t, judgeBusySource(t, opts, late, least*11/10, span).err,
		"ten percent of margin outlasts the same timing")
}

// A ceiling that holds one release interval of a busy source's writes but
// not one grace of them is crossed for good: each release leaves the slot
// holding a release interval and a cycle of writes, still over it, so the
// run ends wal-retained once the grace has passed, with the stream only a
// cycle behind.
func TestSlotLagGraceEndsABusySourceWhoseCeilingHoldsLessThanOneGraceOfWrites(t *testing.T) {
	opts := CatchupOptions{}.withDefaults()
	ceiling := busySourceBytes(busySourceSnapshotInterval)
	run := judgeBusySource(t, opts, onTime(opts), ceiling, 10*busySourceSnapshotInterval)
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, run.err, &exceeded)
	assert.Equal(t, decode.SlotLagWALRetained, exceeded.Cause)
	assert.Equal(t, busySourceBytes(opts.Interval), exceeded.Unconfirmed, "the stream is one cycle behind")
	assert.Greater(t, exceeded.Retained, ceiling)
}

// The Interval credited to the grace is bounded: the longest one accepted
// is the server's release interval, so the grace is at most twice it, and
// a nanosecond more is refused rather than let stretch the grace.
func TestCatchupOptionsBoundTheIntervalCreditedToTheGrace(t *testing.T) {
	longest := CatchupOptions{Interval: MaxCatchupInterval}.withDefaults()
	require.NoError(t, longest.validate())
	assert.Equal(t, 30*time.Second, slotLagGrace(longest))

	over := CatchupOptions{Interval: MaxCatchupInterval + time.Nanosecond}.withDefaults()
	require.ErrorIs(t, over.validate(), ErrInvalidCatchupOptions)

	_, err := NewCatchup(&decode.Stream{}, &Flusher{}, positionStub{}, CatchupOptions{Interval: time.Hour})
	require.ErrorIs(t, err, ErrInvalidCatchupOptions)
}

func TestCatchupOptionsDefaults(t *testing.T) {
	opts := CatchupOptions{}.withDefaults()
	assert.Equal(t, DefaultCatchupInterval, opts.Interval)
	assert.Equal(t, DefaultCatchupMaxChanges, opts.MaxChanges)
	assert.Equal(t, progress.WallClock{}, opts.Clock)
	assert.Equal(t, decode.DefaultSlotLagCeiling, opts.SlotLagCeiling)
	assert.Equal(t, DefaultDeadlockAttempts, opts.DeadlockAttempts)
	require.NotNil(t, opts.Logger, "a caller that wired no logger gets a discarding one")
	assert.Equal(t, slog.DiscardHandler, opts.Logger.Handler())
	assert.Equal(t, DefaultDeadlockBackoff, opts.DeadlockBackoff)
	require.NoError(t, opts.validate())

	logger := slog.New(slog.DiscardHandler)
	given := CatchupOptions{Interval: time.Second, MaxChanges: 5, SlotLagCeiling: 1 << 20, DeadlockAttempts: 3, DeadlockBackoff: time.Minute, Logger: logger}.withDefaults()
	assert.Equal(t, time.Second, given.Interval)
	assert.Equal(t, 5, given.MaxChanges)
	assert.Equal(t, int64(1<<20), given.SlotLagCeiling)
	assert.Equal(t, 3, given.DeadlockAttempts)
	assert.Same(t, logger, given.Logger)
	assert.Equal(t, time.Minute, given.DeadlockBackoff)
}

// An interval the clock cannot time or one that would stretch the slot
// lag grace past its bound, a change bound that would never let
// a cycle end, a slot lag ceiling that would abort every slot, or a
// deadlock bound below one, which counts no flushes at all, or a negative
// deadlock backoff, which would retry before the deadlock, is refused
// rather than defaulted; the ceiling cannot be switched off. Zero takes
// the default deadlock bound, and one, the least, is accepted.
func TestCatchupOptionsRefuseUnboundedValues(t *testing.T) {
	cases := map[string]CatchupOptions{
		"interval below a millisecond": {Interval: 500 * time.Microsecond},
		"interval above the maximum":   {Interval: MaxCatchupInterval + time.Millisecond},
		"negative max changes":         {MaxChanges: -1},
		"negative slot lag ceiling":    {SlotLagCeiling: -1},
		"negative deadlock attempts":   {DeadlockAttempts: -1},
		"negative deadlock backoff":    {DeadlockBackoff: -time.Millisecond},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, opts.withDefaults().validate(), ErrInvalidCatchupOptions)
		})
	}
}

// A deadlock bound of one is accepted: the catch-up ends on the first
// flush the server rolls back as the deadlock victim, without a retry.
func TestCatchupOptionsAcceptOneDeadlockAttempt(t *testing.T) {
	opts := CatchupOptions{DeadlockAttempts: 1}.withDefaults()
	require.NoError(t, opts.validate())
	assert.Equal(t, 1, opts.DeadlockAttempts)
}

// A catch-up without its stream, its flusher, or the copier's position is a
// programming error reported as an invariant violation, not a nil
// dereference in Run.
func TestNewCatchupRefusesMissingParts(t *testing.T) {
	_, err := NewCatchup(nil, &Flusher{}, positionStub{}, CatchupOptions{})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Contains(t, err.Error(), "ST-4")

	_, err = NewCatchup(&decode.Stream{}, nil, positionStub{}, CatchupOptions{})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Contains(t, err.Error(), "ST-6")

	_, err = NewCatchup(&decode.Stream{}, &Flusher{}, nil, CatchupOptions{})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Contains(t, err.Error(), "CO-4")

	_, err = NewCatchup(&decode.Stream{}, &Flusher{}, positionStub{}, CatchupOptions{Interval: time.Nanosecond})
	require.ErrorIs(t, err, ErrInvalidCatchupOptions)
}

// The confirmed position is the delivered one unless the buffer still
// holds an entry from below it: then it is that entry's first position, so
// a stream reopened there replays the transaction the entry came from. A
// pending entry above delivered — one whose transaction committed above
// the position a later keepalive raised delivered to cannot exist, but a
// pending position equal to delivered can — never raises the bound.
func TestConfirmBoundIsDeliveredLoweredToTheOldestPending(t *testing.T) {
	assert.Equal(t, decode.LSN(100), confirmBound(100, 0, false), "nothing pending: delivered")
	assert.Equal(t, decode.LSN(40), confirmBound(100, 40, true), "a pending entry below delivered bounds the confirmation")
	assert.Equal(t, decode.LSN(100), confirmBound(100, 100, true), "a pending entry at delivered leaves it")
	assert.Equal(t, decode.LSN(100), confirmBound(100, 150, true), "a pending entry above delivered never raises it")
}

// A stop is the caller's once ctx has ended: the stream's receive failing
// on the ended context is the stop itself and is folded into it, while a
// breach raised in the same cycle travels with the stop so a caller still
// sees it. On a live ctx the cycle's error is returned as it is.
func TestStoppedKeepsACycleErrorThatIsNotTheStop(t *testing.T) {
	live := t.Context()
	breach := fmt.Errorf("%w (CO-8): image omits a column", ErrInvariantViolation)
	assert.Same(t, breach, stopped(live, "s", "t", breach), "a live ctx leaves the cycle's error alone")

	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("owner is ready to cut over")
	cancel(cause)
	receive := fmt.Errorf("receive from slot: %w", context.Canceled)
	err := stopped(ctx, "s", "t", receive)
	assert.ErrorIs(t, err, cause)
	assert.ErrorIs(t, err, context.Canceled, "a stop with the owner's own cause is still a cancellation")
	assert.EqualError(t, err, "catch-up on s.t stopped: owner is ready to cut over (context canceled)", "the receive error is the stop itself")

	err = stopped(ctx, "s", "t", breach)
	assert.ErrorIs(t, err, cause, "the stop is still reported")
	assert.ErrorIs(t, err, ErrInvariantViolation, "the breach travels with it")
	assert.ErrorIs(t, err, breach)
}

// An ended ctx is reported by its cause; a cause the owner chose is wrapped
// with ctx.Err() so the error is also the cancellation it is, while a plain
// cancel or a passed deadline is reported as it stands, not doubled.
func TestEndedWithKeepsACancellationReadableAsOne(t *testing.T) {
	plain, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Equal(t, context.Canceled, endedWith(plain))

	expired, cancelExpired := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancelExpired()
	<-expired.Done()
	assert.Equal(t, context.DeadlineExceeded, endedWith(expired))

	caused, cancelCaused := context.WithCancelCause(t.Context())
	cause := errors.New("owner is ready to cut over")
	cancelCaused(cause)
	err := endedWith(caused)
	assert.ErrorIs(t, err, cause)
	assert.ErrorIs(t, err, context.Canceled)
	assert.EqualError(t, err, "owner is ready to cut over (context canceled)")

	interrupted := fmt.Errorf("interrupted by signal: %w", context.Canceled)
	wrapped, cancelWrapped := context.WithCancelCause(t.Context())
	cancelWrapped(interrupted)
	assert.Same(t, interrupted, endedWith(wrapped), "a cause that is already a cancellation is reported as it stands")
}

// The copy has landed when its cut frontier is complete and no claimed
// chunk is unlanded; a complete frontier with a chunk in flight, or a
// frontier short of the end, still has a chunk that can resolve a refusal.
func TestCopyLanded(t *testing.T) {
	chunk, err := copier.NewChunk(2, 2)
	require.NoError(t, err)
	assert.True(t, copyLanded(copier.Position{Cut: copier.NewWatermark(math.MaxInt64)}))
	assert.False(t, copyLanded(copier.Position{Cut: copier.NewWatermark(math.MaxInt64), InFlight: []copier.Chunk{chunk}}), "a chunk in flight")
	assert.False(t, copyLanded(copier.Position{Cut: copier.NewWatermark(500)}), "the frontier is short of the end")
	assert.False(t, copyLanded(copier.Position{}), "nothing claimed")
}

// Lag is the WAL between the confirmed position and the server's write
// position as the last cycle measured it, and zero before a cycle has
// measured one or once the confirmation has reached it.
func TestStatusLag(t *testing.T) {
	assert.Zero(t, Status{Confirmed: 50}.Lag(), "no measurement yet")
	assert.Zero(t, Status{Confirmed: 80, WALEnd: 80}.Lag(), "caught up")
	assert.Equal(t, uint64(30), Status{Confirmed: 50, WALEnd: 80}.Lag())
}

// A catch-up's work is its status folded into the counters the tracker
// publishes: buffered keys and lag at the end of the last cycle, applied
// changes since Run began.
func TestCatchupWorkMirrorsStatus(t *testing.T) {
	c := &Catchup{status: Status{Applied: 12, Buffered: 3, Confirmed: 50, WALEnd: 80}}
	work, err := c.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChangesApplied: 12, ChangesBuffered: 3, LagBytes: 30}, work)
}

type positionStub struct{}

func (positionStub) Position() copier.Position { return copier.Position{} }
