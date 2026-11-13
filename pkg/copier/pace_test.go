package copier

import (
	"context"
	"errors"
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
	return nil
}

// pacedCopier is a copier holding only what awaitPace reads: the pace,
// with its defaults, over the fake.
func pacedCopier(t *testing.T, f *paceFake, pace Pace) *Copier {
	t.Helper()
	pace.Backlog = f.backlog
	pace.Sleep = f.sleep
	opts := Options{Pace: pace}.withDefaults()
	require.NoError(t, opts.validate())
	return &Copier{opts: opts}
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
	assert.Equal(t, Paced{Waits: 2, Expired: 2, Waited: 200 * time.Millisecond}, c.Paced(), "every chunk claimed under the backlog waits its own MaxWait")
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
