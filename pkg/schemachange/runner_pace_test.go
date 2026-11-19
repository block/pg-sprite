package schemachange

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/progress"
)

// fixedStatus is a catch-up whose last cycle left the status it holds.
type fixedStatus struct {
	status applier.Status
}

func (f fixedStatus) Status() applier.Status { return f.status }

// The lag reads zero until a catch-up is named, so a copier built before
// its capture is not held back by a reading that does not exist yet; from
// then on it is the catch-up's own figure, the server's write position
// less the confirmed one, and zero once the confirmation has caught up.
func TestStreamLagReadsTheCatchupItFollows(t *testing.T) {
	lag := &streamLag{}
	assert.Zero(t, lag.bytes(), "no catch-up to read yet")

	lag.follow(fixedStatus{status: applier.Status{Confirmed: 1000, WALEnd: 1700}})
	assert.Equal(t, uint64(700), lag.bytes())

	lag.follow(fixedStatus{status: applier.Status{Confirmed: 1700, WALEnd: 1700}})
	assert.Zero(t, lag.bytes())
}

// The copy waits above a quarter of the slot lag ceiling: under the
// default ceiling of one GiB that is 256 MiB, a ceiling the run names is
// quartered the same way, and a ceiling too small to quarter still leaves
// a mark of one byte for the copier's pace to accept.
func TestCopyPaceMarkIsAQuarterOfTheSlotLagCeiling(t *testing.T) {
	assert.Equal(t, uint64(256<<20), copyPaceMark(RunOptions{}.slotLagCeiling()))
	assert.Equal(t, int64(decode.DefaultSlotLagCeiling), RunOptions{}.slotLagCeiling())

	named := RunOptions{Catchup: applier.CatchupOptions{SlotLagCeiling: 64 << 20}}
	assert.Equal(t, uint64(16<<20), copyPaceMark(named.slotLagCeiling()))

	assert.Equal(t, uint64(1), copyPaceMark(3))
	assert.Equal(t, uint64(1), copyPaceMark(1))
}

// A live run's copier options are the run's own with the pace laid over:
// the backlog is the lag of whichever catch-up the run then follows, the
// mark is the run's share of its ceiling, and the wait uses the run's poll
// and its injected sleep. The options pass the copier's own validation,
// the run's other copier settings are carried unchanged, and the run's
// tracker is the copy's tracker, as it is for a quiesced copy.
func TestPacedCopierPacesByTheRunsOwnLag(t *testing.T) {
	stopped := errors.New("slept")
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	r, err := NewRunner(dbconn.Config{}, &checkpoint.Store{}, RunOptions{
		Copier:  copier.Options{Workers: 3},
		Catchup: applier.CatchupOptions{SlotLagCeiling: 200},
		Live:    checksum.LiveOptions{Poll: 7 * time.Millisecond},
		Sleep:   func(context.Context, time.Duration) error { return stopped },
		Tracker: tracker,
		Step:    1,
	})
	require.NoError(t, err)

	lag := &streamLag{}
	opts := r.pacedCopier(lag)
	require.NoError(t, opts.Validate())

	assert.Equal(t, 3, opts.Workers)
	assert.Same(t, tracker, opts.Tracker, "the run's tracker reports the paced copy")
	assert.Equal(t, uint64(50), opts.Pace.HighWater)
	assert.Equal(t, 7*time.Millisecond, opts.Pace.Poll)
	assert.ErrorIs(t, opts.Pace.Sleep(t.Context(), time.Second), stopped, "the wait is the run's injected sleep")

	assert.Zero(t, opts.Pace.Backlog(), "before the capture starts")
	lag.follow(fixedStatus{status: applier.Status{Confirmed: 50, WALEnd: 175}})
	assert.Equal(t, uint64(125), opts.Pace.Backlog(), "the catch-up named after the options were made")

	assert.Nil(t, r.opts.Copier.Pace.Backlog, "the run's own options are left unpaced for a quiesced copy")
}

// A run with no hook reads its own catch-up's lag: paceSource hands the
// copy the catch-up it was given, unwrapped.
func TestPaceSourceIsTheCatchupWhenNoHookIsSet(t *testing.T) {
	r, err := NewRunner(dbconn.Config{}, &checkpoint.Store{}, RunOptions{})
	require.NoError(t, err)
	catchup := &applier.Catchup{}
	got, ok := r.paceSource(catchup).(*applier.Catchup)
	require.True(t, ok, "the copy reads the catch-up itself")
	assert.Same(t, catchup, got)
}

// PaceOverMark reads the lag one byte over the run's pace mark for exactly
// the readings it is given, calling onOver at each, and the catch-up's own
// lag after them.
func TestPaceOverMarkReadsOverTheMarkForItsReadingsOnly(t *testing.T) {
	r, err := NewRunner(dbconn.Config{}, &checkpoint.Store{}, RunOptions{
		Catchup: applier.CatchupOptions{SlotLagCeiling: 200},
	})
	require.NoError(t, err)
	var over atomic.Int64
	PaceOverMark(r, 3, func() { over.Add(1) })
	mark := copyPaceMark(200)
	require.Equal(t, uint64(50), mark)

	lag := &streamLag{}
	lag.follow(r.paceWrap(fixedStatus{status: applier.Status{Confirmed: 1000, WALEnd: 1010}}))
	for i := range 3 {
		assert.Equal(t, mark+1, lag.bytes(), "reading %d is one byte over the mark", i+1)
	}
	assert.Equal(t, uint64(10), lag.bytes(), "the catch-up's own lag once the readings are used")
	assert.Equal(t, uint64(10), lag.bytes())
	assert.Equal(t, int64(3), over.Load())
}

// Workers read the lag concurrently, so the readings over the mark are
// handed out once each, however many workers race for them.
func TestPaceOverMarkHandsOutEachReadingOnce(t *testing.T) {
	r, err := NewRunner(dbconn.Config{}, &checkpoint.Store{}, RunOptions{
		Catchup: applier.CatchupOptions{SlotLagCeiling: 200},
	})
	require.NoError(t, err)
	PaceOverMark(r, 3, nil)
	lag := &streamLag{}
	lag.follow(r.paceWrap(fixedStatus{}))
	var wg sync.WaitGroup
	var over atomic.Int64
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if lag.bytes() > copyPaceMark(200) {
					over.Add(1)
				}
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int64(3), over.Load())
}

// A run with the hook set reads its catch-up through it, and each run's
// catch-up gets readings of its own, so a second run on the same Runner is
// held as the first was.
func TestPaceOverMarkGivesEachRunItsOwnReadings(t *testing.T) {
	r, err := NewRunner(dbconn.Config{}, &checkpoint.Store{}, RunOptions{})
	require.NoError(t, err)
	PaceOverMark(r, 1, nil)
	first, second := r.paceSource(&applier.Catchup{}), r.paceSource(&applier.Catchup{})
	assert.Positive(t, first.Status().Lag())
	assert.Positive(t, second.Status().Lag())
	assert.Zero(t, first.Status().Lag())
}
