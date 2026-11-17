package checksum

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// confirmedAt is a ConfirmedSource pinned at one position.
type confirmedAt decode.LSN

func (c confirmedAt) Confirmed() decode.LSN { return decode.LSN(c) }

func TestLiveOptionsDefaults(t *testing.T) {
	opts := LiveOptions{Confirmed: confirmedAt(0)}.withDefaults()
	assert.Equal(t, DefaultSettleAttempts, opts.Attempts)
	assert.Equal(t, DefaultSettleWait, opts.SettleWait)
	assert.Equal(t, DefaultSettlePoll, opts.Poll)
	assert.NotNil(t, opts.Sleep)
	require.NoError(t, opts.validate(DivergenceAbort))

	given := LiveOptions{Confirmed: confirmedAt(0), Attempts: 5, SettleWait: time.Minute, Poll: time.Second}.withDefaults()
	assert.Equal(t, 5, given.Attempts)
	assert.Equal(t, time.Minute, given.SettleWait)
	assert.Equal(t, time.Second, given.Poll)
}

// A live pass cannot settle anything without the catch-up's position, and
// a bound the server or the loop would read as none is refused rather than
// defaulted.
func TestLiveOptionsRefuseUnboundedValues(t *testing.T) {
	cases := map[string]LiveOptions{
		"no confirmed source":             {},
		"negative attempts":               {Confirmed: confirmedAt(0), Attempts: -1},
		"negative carry keys":             {Confirmed: confirmedAt(0), CarryKeys: -1},
		"carry keys above the ceiling":    {Confirmed: confirmedAt(0), CarryKeys: MaxCarryKeys + 1},
		"settle wait below a millisecond": {Confirmed: confirmedAt(0), SettleWait: time.Microsecond},
		"poll below a millisecond":        {Confirmed: confirmedAt(0), Poll: time.Nanosecond},
		"settle wait shorter than poll":   {Confirmed: confirmedAt(0), SettleWait: 10 * time.Millisecond, Poll: 20 * time.Millisecond},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, opts.withDefaults().validate(DivergenceAbort), ErrInvalidOptions)
		})
	}
}

// The carry limit's range is inclusive: a pass may be told to leave exactly
// MaxCarryKeys keys, and only one more is refused.
func TestLiveOptionsAcceptTheCarryCeiling(t *testing.T) {
	live := LiveOptions{Confirmed: confirmedAt(0), CarryKeys: MaxCarryKeys}.withDefaults()
	assert.NoError(t, live.validate(DivergenceAbort))
}

// pauseNever is a FlushGate whose hold is a no-op, for options tests that
// only need the field set.
type pauseNever struct{}

func (pauseNever) Pause(context.Context) (func(), error) { return func() {}, nil }

// Only a repair recopies the shadow, so only a pass under repair needs the
// gate that holds the catch-up's flushes off it; a pass under abort with no
// gate is valid, and one under repair with no gate is refused before it
// reads anything.
func TestLiveOptionsRequireTheFlushGateOnlyForRepair(t *testing.T) {
	without := LiveOptions{Confirmed: confirmedAt(0)}.withDefaults()
	require.NoError(t, without.validate(DivergenceAbort))
	assert.ErrorIs(t, without.validate(DivergenceRepair), ErrInvalidOptions)

	with := LiveOptions{Confirmed: confirmedAt(0), Flushes: pauseNever{}}.withDefaults()
	require.NoError(t, with.validate(DivergenceRepair))
}

// An unsettled chunk unwraps to the sentinel and says why in words an
// operator can act on: which position the catch-up was short of, or how
// many rereads the chunk outran and how many keys were still moving, and
// what to do next in either case.
func TestUnresolvedErrorWording(t *testing.T) {
	chunk, err := copier.NewChunk(1001, 2000)
	require.NoError(t, err)
	lagging := &UnresolvedError{Chunk: chunk, Cause: CauseLagging, Rereads: 1, Captured: 0x1000000, Confirmed: 0x800000}
	require.ErrorIs(t, lagging, ErrVerificationUnresolved)
	assert.EqualError(t, lagging, "chunk [1001, 2000] read at 0/1000000 is unsettled: the catch-up has confirmed only 0/800000 after 1 reread; run the pass again once the catch-up has confirmed past the read")

	hot := &UnresolvedError{Chunk: chunk, Cause: CauseHot, Rereads: 3, Keys: 2, Source: Digest{Rows: 1000}, Shadow: Digest{Rows: 998}, Captured: 0x1000000}
	require.ErrorIs(t, hot, ErrVerificationUnresolved)
	assert.EqualError(t, hot, "chunk [1001, 2000] kept changing through 3 rereads: 2 keys still changing, source 1000 rows, shadow 998 rows at 0/1000000; the source is changing faster than the pass can compare it, so run the pass again when it is quieter")

	one := &UnresolvedError{Chunk: chunk, Cause: CauseHot, Rereads: 1, Keys: 1, Source: Digest{Rows: 1000}, Shadow: Digest{Rows: 1000}, Captured: 0x1000000}
	assert.EqualError(t, one, "chunk [1001, 2000] kept changing through 1 reread: 1 key still changing, source 1000 rows, shadow 1000 rows at 0/1000000; the source is changing faster than the pass can compare it, so run the pass again when it is quieter")
}

// The wall-clock sleep returns early, with the context's error, when the
// context ends first.
func TestSleepContextReturnsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := sleepContext(ctx, time.Hour)
	assert.True(t, errors.Is(err, context.Canceled))
}
