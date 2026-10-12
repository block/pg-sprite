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
	require.NoError(t, opts.validate())

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
		"settle wait below a millisecond": {Confirmed: confirmedAt(0), SettleWait: time.Microsecond},
		"poll below a millisecond":        {Confirmed: confirmedAt(0), Poll: time.Nanosecond},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, opts.withDefaults().validate(), ErrInvalidOptions)
		})
	}
}

// Two reads of a differing chunk, the second taken after the catch-up
// confirmed past the first, are judged together: agreement or the shadow
// reaching the first read's source settles the chunk, movement on either
// side means another read, and stillness on both sides with a difference
// is the divergence.
func TestJudgeVerdicts(t *testing.T) {
	chunk, err := copier.NewChunk(1, 1000)
	require.NoError(t, err)
	a := Digest{Rows: 1000, Hash: "a"}
	b := Digest{Rows: 1000, Hash: "b"}
	c := Digest{Rows: 999, Hash: "c"}
	d := Digest{Rows: 1000, Hash: "d"}
	read := func(source, shadow Digest) observation {
		return observation{chunk: chunk, source: source, shadow: shadow}
	}
	cases := map[string]struct {
		prior, now observation
		want       verdict
	}{
		"shadow caught up to the moved source":     {read(a, b), read(c, c), verdictEqual},
		"shadow reached what the sweep saw":        {read(a, b), read(c, a), verdictSettled},
		"source moved and shadow still behind":     {read(a, b), read(c, b), verdictHot},
		"source still and shadow moved elsewhere":  {read(a, b), read(a, d), verdictHot},
		"both moved and still apart":               {read(a, b), read(c, d), verdictHot},
		"both still across the confirmed position": {read(a, b), read(a, b), verdictDiverged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, judge(tc.prior, tc.now))
		})
	}
}

// An unsettled chunk unwraps to the sentinel and says why in words an
// operator can act on: which position the catch-up was short of, or how
// many rereads the chunk outran.
func TestUnresolvedErrorWording(t *testing.T) {
	chunk, err := copier.NewChunk(1001, 2000)
	require.NoError(t, err)
	lagging := &UnresolvedError{Chunk: chunk, Cause: CauseLagging, Rereads: 1, Captured: 0x1000000, Confirmed: 0x800000}
	require.ErrorIs(t, lagging, ErrVerificationUnresolved)
	assert.EqualError(t, lagging, "chunk [1001, 2000] read at 0/1000000 is unsettled: the catch-up has confirmed only 0/800000 after 1 rereads")

	hot := &UnresolvedError{Chunk: chunk, Cause: CauseHot, Rereads: 3, Source: Digest{Rows: 1000}, Shadow: Digest{Rows: 998}, Captured: 0x1000000}
	require.ErrorIs(t, hot, ErrVerificationUnresolved)
	assert.EqualError(t, hot, "chunk [1001, 2000] changed on every one of 3 rereads: source 1000 rows, shadow 998 rows at 0/1000000")
}

// The wall-clock sleep returns early, with the context's error, when the
// context ends first.
func TestSleepContextReturnsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := sleepContext(ctx, time.Hour)
	assert.True(t, errors.Is(err, context.Canceled))
}
