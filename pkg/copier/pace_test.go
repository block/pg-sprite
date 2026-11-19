package copier

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// paceFake drives a Pace without real time: readings holds the backlog
// each successive read returns, the last one standing for every read after
// it, and slept records every wait the pace asked for.
type paceFake struct {
	readings []uint64
	reads    int
	slept    []time.Duration
	// sleepErr, when set, is what the wait returns instead of waiting.
	sleepErr error
	// onSleep, when set, runs during every wait: what other workers do
	// while this one is held back.
	onSleep func()
}

func (f *paceFake) backlog() uint64 {
	i := min(f.reads, len(f.readings)-1)
	f.reads++
	return f.readings[i]
}

func (f *paceFake) sleep(_ context.Context, d time.Duration) error {
	if f.sleepErr != nil {
		return f.sleepErr
	}
	f.slept = append(f.slept, d)
	if f.onSleep != nil {
		f.onSleep()
	}
	return nil
}

// pacedCopier is a copier holding only what awaitPace reads: the pace,
// with its defaults, over the fake, and a chunker with chunks left to cut.
func pacedCopier(t *testing.T, f *paceFake, pace Pace) *Copier {
	t.Helper()
	pace.Backlog = f.backlog
	pace.Sleep = f.sleep
	opts := Options{Pace: pace}.withDefaults()
	require.NoError(t, opts.validate())
	return &Copier{opts: opts, chunker: &Chunker{}}
}

// A copy with no pace, and a paced copy whose backlog reads at the mark,
// claim at once: the mark is the most backlog the copy runs under, so only
// a reading above it waits.
func TestAwaitPaceClaimsAtOnceWhenTheBacklogIsWithinTheMark(t *testing.T) {
	unpaced := &Copier{opts: Options{}.withDefaults()}
	require.NoError(t, unpaced.awaitPace(t.Context()))
	assert.Equal(t, Paced{}, unpaced.Paced())

	f := &paceFake{readings: []uint64{100}}
	c := pacedCopier(t, f, Pace{HighWater: 100})
	require.NoError(t, c.awaitPace(t.Context()))
	assert.Empty(t, f.slept, "a backlog at the mark is not over it")
	assert.Equal(t, Paced{}, c.Paced())
}

// A backlog over the mark holds the worker, which reads again every Poll
// and claims on the first reading back within the mark: here the third,
// after two waits. The wait is counted once, with the time it took.
func TestAwaitPaceWaitsUntilTheBacklogFalls(t *testing.T) {
	f := &paceFake{readings: []uint64{101, 300, 100}}
	c := pacedCopier(t, f, Pace{HighWater: 100, Poll: 50 * time.Millisecond, MaxWait: time.Second})

	require.NoError(t, c.awaitPace(t.Context()))

	assert.Equal(t, []time.Duration{50 * time.Millisecond, 50 * time.Millisecond}, f.slept)
	assert.Equal(t, 3, f.reads, "one reading before the wait and one after each poll")
	assert.Equal(t, Paced{Waits: 1, Waited: 100 * time.Millisecond}, c.Paced())
}

// The wait is a brake, not a gate: a backlog that never falls holds the
// worker for MaxWait and no longer, the last poll cut short so the polls
// sum to it, and the worker then claims with the backlog still over the
// mark. That wait is counted as expired.
func TestAwaitPaceClaimsAfterMaxWaitWithTheBacklogStillOver(t *testing.T) {
	f := &paceFake{readings: []uint64{500}}
	c := pacedCopier(t, f, Pace{HighWater: 100, Poll: 40 * time.Millisecond, MaxWait: 100 * time.Millisecond})

	require.NoError(t, c.awaitPace(t.Context()))

	assert.Equal(t, []time.Duration{40 * time.Millisecond, 40 * time.Millisecond, 20 * time.Millisecond}, f.slept)
	assert.Equal(t, Paced{Waits: 1, Expired: 1, Waited: 100 * time.Millisecond}, c.Paced())

	require.NoError(t, c.awaitPace(t.Context()))
	assert.Equal(t, Paced{Waits: 1, Expired: 1, Waited: 100 * time.Millisecond}, c.Paced(), "a wait that ran out releases the pace, so the next claim does not wait")
	assert.Len(t, f.slept, 3)
}

// A released pace arms again on the first reading back within the mark:
// the backlog then rising over it holds the next claim once more. Here the
// first wait runs out at 500, a claim at 500 goes straight through, a
// claim at 100 re-arms the pace, and the next claim at 500 waits again.
func TestAwaitPaceArmsAgainOnceTheBacklogIsBackWithinTheMark(t *testing.T) {
	// Each claim reads once before it waits; a wait of 100ms polls at 50ms
	// and 100ms, reading after each.
	f := &paceFake{readings: []uint64{500, 500, 500, 500, 100, 500, 500, 100}}
	c := pacedCopier(t, f, Pace{HighWater: 100, Poll: 50 * time.Millisecond, MaxWait: 100 * time.Millisecond})

	require.NoError(t, c.awaitPace(t.Context()), "first claim: waits 100ms and runs out")
	require.NoError(t, c.awaitPace(t.Context()), "second claim: the pace is released")
	require.NoError(t, c.awaitPace(t.Context()), "third claim: the reading is within the mark, which arms the pace")
	require.NoError(t, c.awaitPace(t.Context()), "fourth claim: over the mark again, so it waits until the reading falls")

	assert.Equal(t, Paced{Waits: 2, Expired: 1, Waited: 200 * time.Millisecond}, c.Paced())
	assert.Equal(t, 8, f.reads)
}

// A backlog that falls during a wait but is still over the mark when the
// wait runs out is draining, so the copy is its cause: the worker claims
// one chunk, but the pace stays armed and the next claim waits again. Here
// the first wait falls from 900 to 500 against a mark of 100, and the
// second claim, reading 500, waits rather than going straight through.
func TestAwaitPaceStaysArmedWhileTheBacklogFalls(t *testing.T) {
	f := &paceFake{readings: []uint64{900, 700, 500}}
	c := pacedCopier(t, f, Pace{HighWater: 100, Poll: 50 * time.Millisecond, MaxWait: 100 * time.Millisecond})

	require.NoError(t, c.awaitPace(t.Context()), "first claim: waits 100ms, the backlog falls to 500 and the wait runs out")
	require.NoError(t, c.awaitPace(t.Context()), "second claim: the pace is still armed, so it waits")

	assert.Equal(t, Paced{Waits: 2, Expired: 2, Waited: 200 * time.Millisecond}, c.Paced())
	assert.Len(t, f.slept, 4)
}

// A reading within the mark arms the pace even when it ends a wait that
// another worker's expiry released meanwhile: here a second worker's wait
// runs out while this one polls, this one then reads 100 and claims, and
// the next claim at 500 waits instead of going straight through.
func TestAwaitPaceArmsOnAReadingWithinTheMarkDuringAWait(t *testing.T) {
	f := &paceFake{readings: []uint64{500, 100, 500, 100}}
	c := pacedCopier(t, f, Pace{HighWater: 100, Poll: 50 * time.Millisecond, MaxWait: time.Second})
	f.onSleep = func() { c.paceReleased.Store(true) }

	require.NoError(t, c.awaitPace(t.Context()), "first claim: another worker's wait runs out during this one's poll")
	f.onSleep = nil
	require.NoError(t, c.awaitPace(t.Context()), "second claim: the reading of 100 armed the pace, so 500 waits")

	assert.Equal(t, Paced{Waits: 2, Waited: 100 * time.Millisecond}, c.Paced())
}

// A worker held back stops waiting once every chunk has been claimed,
// since there is nothing left for it to hold back: here the last chunk is
// claimed by another worker during this one's first poll, and the wait
// ends there instead of running to MaxWait. A worker that finds every
// chunk claimed before it would wait does not wait at all.
func TestAwaitPaceStopsWaitingOnceEveryChunkIsClaimed(t *testing.T) {
	f := &paceFake{readings: []uint64{500}}
	c := pacedCopier(t, f, Pace{HighWater: 100, Poll: 50 * time.Millisecond, MaxWait: time.Second})
	f.onSleep = func() { c.chunker.advance(math.MaxInt64) }

	require.NoError(t, c.awaitPace(t.Context()))
	assert.Equal(t, []time.Duration{50 * time.Millisecond}, f.slept)
	assert.Equal(t, Paced{Waits: 1, Waited: 50 * time.Millisecond}, c.Paced(), "a wait the drained chunker ended is not expired")

	require.NoError(t, c.awaitPace(t.Context()))
	assert.Len(t, f.slept, 1, "with every chunk claimed the next claim does not wait")
	assert.Equal(t, int64(1), c.Paced().Waits)
}

// A run that ends while a worker is held back ends the wait: the worker
// gets the context's error and claims nothing.
func TestAwaitPaceReturnsTheContextsErrorWhenTheRunEnds(t *testing.T) {
	stopped := errors.New("run stopped")
	f := &paceFake{readings: []uint64{500}, sleepErr: stopped}
	c := pacedCopier(t, f, Pace{HighWater: 100})

	require.ErrorIs(t, c.awaitPace(t.Context()), stopped)
	assert.Equal(t, Paced{Waits: 1}, c.Paced(), "a wait that was cut short is neither expired nor timed")
}

// A pace takes its poll, its longest wait, and its clock from the defaults
// when it names only what to read and the mark; the zero Pace stays zero,
// so a copy that is not paced carries nothing.
func TestPaceDefaults(t *testing.T) {
	zero := Options{}.withDefaults()
	assert.Nil(t, zero.Pace.Backlog)
	assert.Nil(t, zero.Pace.Sleep)
	assert.Zero(t, zero.Pace.Poll)
	assert.Zero(t, zero.Pace.MaxWait)

	paced := Options{Pace: Pace{Backlog: func() uint64 { return 0 }, HighWater: 1}}.withDefaults()
	assert.Equal(t, DefaultPacePoll, paced.Pace.Poll)
	assert.Equal(t, DefaultPaceMaxWait, paced.Pace.MaxWait)
	assert.NotNil(t, paced.Pace.Sleep)
	require.NoError(t, paced.validate())

	given := Options{Pace: Pace{Backlog: func() uint64 { return 0 }, HighWater: 1, Poll: time.Second, MaxWait: time.Hour}}.withDefaults()
	assert.Equal(t, time.Second, given.Pace.Poll)
	assert.Equal(t, time.Hour, given.Pace.MaxWait)
}

// A pace that could not do what it says is refused: a mark with nothing to
// read against it would pace nothing while looking configured, a reading
// with no mark would hold the copy on any backlog at all, and a negative
// poll or wait is not a duration to wait for.
func TestPaceRefusesWhatItCouldNotHonour(t *testing.T) {
	backlog := func() uint64 { return 0 }
	cases := map[string]Pace{
		"mark without a backlog":     {HighWater: 100},
		"poll without a backlog":     {Poll: time.Second},
		"max wait without a backlog": {MaxWait: time.Second},
		"sleep without a backlog":    {Sleep: sleepContext},
		"backlog without a mark":     {Backlog: backlog},
		"negative poll":              {Backlog: backlog, HighWater: 100, Poll: -time.Second},
		"negative max wait":          {Backlog: backlog, HighWater: 100, MaxWait: -time.Second},
	}
	for name, pace := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, Options{Pace: pace}.withDefaults().validate(), ErrInvalidOptions)
		})
	}
}
