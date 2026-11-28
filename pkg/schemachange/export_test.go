package schemachange

import (
	"sync/atomic"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/decode"
)

// This file exposes test-only hooks to the package's external tests; it
// compiles only with them, so nothing here is importable API.

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

// WrapLiveConfirmed makes r's live verification pass read its confirmed
// position from what wrap returns for the run's catch-up. wrap is called
// once per pass, just before the pass begins, so it can also hold the
// catch-up's flushes with Pause and decide, at every read of the
// position the pass makes before a reread, what the shadow holds when
// the reread reads it.
func WrapLiveConfirmed(r *Runner, wrap func(*applier.Catchup) checksum.ConfirmedSource) {
	r.liveWrap = wrap
}

// JoinDiscardRelease is the fold Runner.Discard makes of its table lock's
// release error into its own outcome, so a test can drive a release
// failure that a live lock session cannot be made to produce on demand.
func JoinDiscardRelease(discardErr error, schema, table string, releaseErr error) error {
	return joinDiscardRelease(discardErr, schema, table, releaseErr)
}
