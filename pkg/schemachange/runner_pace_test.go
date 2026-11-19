package schemachange

import (
	"context"
	"errors"
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

// overMarkStatus reads a catch-up's status with the stream's lag put one
// byte over the copy's pace mark for its first readings, and as the
// catch-up has it after them. onOver, when set, is called at each reading
// over the mark, which is a worker about to be held before its chunk.
type overMarkStatus struct {
	source catchupStatus
	mark   uint64
	left   atomic.Int64
	onOver func()
}

func (s *overMarkStatus) Status() applier.Status {
	status := s.source.Status()
	if s.left.Add(-1) < 0 {
		return status
	}
	status.WALEnd = status.Confirmed + decode.LSN(s.mark) + 1
	if s.onOver != nil {
		s.onOver()
	}
	return status
}

// PaceOverMark makes r's live runs read the stream's lag as over the copy's
// pace mark for the first readings of it, calling onOver at each, and as
// the run's catch-up has it after them. It lets a test drive the copy's
// pace through a whole Run without depending on the stream's lag crossing
// the mark on its own.
func PaceOverMark(r *Runner, readings int64, onOver func()) {
	mark := copyPaceMark(r.opts.slotLagCeiling())
	r.paceWrap = func(c catchupStatus) catchupStatus {
		s := &overMarkStatus{source: c, mark: mark, onOver: onOver}
		s.left.Store(readings)
		return s
	}
}

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
