package applier

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/progress"
)

func TestCatchupOptionsDefaults(t *testing.T) {
	opts := CatchupOptions{}.withDefaults()
	assert.Equal(t, DefaultCatchupInterval, opts.Interval)
	assert.Equal(t, DefaultCatchupMaxChanges, opts.MaxChanges)
	assert.Equal(t, progress.WallClock{}, opts.Clock)
	assert.Equal(t, decode.DefaultSlotLagCeiling, opts.SlotLagCeiling)
	require.NoError(t, opts.validate())

	given := CatchupOptions{Interval: time.Second, MaxChanges: 5, SlotLagCeiling: 1 << 20}.withDefaults()
	assert.Equal(t, time.Second, given.Interval)
	assert.Equal(t, 5, given.MaxChanges)
	assert.Equal(t, int64(1<<20), given.SlotLagCeiling)
}

// An interval the clock cannot time, a change bound that would never let
// a cycle end, or a slot lag ceiling that would abort every slot is refused
// rather than defaulted; the ceiling cannot be switched off.
func TestCatchupOptionsRefuseUnboundedValues(t *testing.T) {
	cases := map[string]CatchupOptions{
		"interval below a millisecond": {Interval: 500 * time.Microsecond},
		"negative max changes":         {MaxChanges: -1},
		"negative slot lag ceiling":    {SlotLagCeiling: -1},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, opts.withDefaults().validate(), ErrInvalidCatchupOptions)
		})
	}
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
	assert.EqualError(t, err, "catch-up on s.t stopped: owner is ready to cut over", "the receive error is the stop itself")

	err = stopped(ctx, "s", "t", breach)
	assert.ErrorIs(t, err, cause, "the stop is still reported")
	assert.ErrorIs(t, err, ErrInvariantViolation, "the breach travels with it")
	assert.ErrorIs(t, err, breach)
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
