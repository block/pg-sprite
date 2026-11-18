package schemachange

import (
	"sync/atomic"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// copyPaceCeilingShare divides the slot lag ceiling into the mark a live
// run's copy is paced at: the copy waits before a chunk while the stream's
// unconfirmed WAL is over the ceiling divided by this. The rest of the
// ceiling is headroom for what a wait cannot hold back: the chunks every
// worker already has in flight, the writes the source takes meanwhile,
// and a reading that is as old as the catch-up cycle in progress, during
// which the lag keeps growing unseen.
const copyPaceCeilingShare = 4

// streamLag reads how much WAL a live run's capture has yet to confirm,
// for the copier's pace. The copier is built before the capture, which
// reads the copier's position, so the catch-up is named afterwards with
// follow; until then the lag reads zero and the copy is not held back.
//
// The reading is the catch-up's own, taken once per catch-up cycle, so it
// is as old as the cycle in progress is long.
type streamLag struct {
	source atomic.Pointer[catchupStatus]
}

// catchupStatus is what streamLag reads of a catch-up: its status as the
// last cycle left it. *applier.Catchup is the one a run follows.
type catchupStatus interface {
	Status() applier.Status
}

// follow names the catch-up whose lag is read from here on.
func (l *streamLag) follow(c catchupStatus) {
	l.source.Store(&c)
}

// bytes is the unconfirmed WAL as the catch-up's last cycle measured it:
// the server's write position less the position the shadow durably holds.
func (l *streamLag) bytes() uint64 {
	source := l.source.Load()
	if source == nil {
		return 0
	}
	return (*source).Status().Lag()
}

// pacedCopier is the run's copier options, with the run's tracker when it
// has one, and the copy paced by lag: a worker waits before a chunk while
// the stream's unconfirmed WAL is over its share of the slot lag ceiling,
// so the copy's own inserts, which the stream's backend has to read past,
// do not carry the slot to the ceiling that ends the run (ST-3). The
// ceiling still ends a run whose lag is not the copy's doing.
func (r *Runner) pacedCopier(lag *streamLag) copier.Options {
	opts := r.copierOptions()
	opts.Pace = copier.Pace{
		Backlog:   lag.bytes,
		HighWater: copyPaceMark(r.opts.slotLagCeiling()),
		Poll:      r.opts.livePoll(),
		Sleep:     r.opts.sleep(),
	}
	return opts
}

// copyPaceMark is the unconfirmed WAL, in bytes, above which a live run's
// copy waits, for a slot lag ceiling: its share of the ceiling, and at
// least one byte so the smallest ceiling still leaves a mark.
func copyPaceMark(ceiling int64) uint64 {
	return uint64(max(ceiling/copyPaceCeilingShare, 1))
}

// slotLagCeiling is the ceiling the run's catch-up judges its slot by,
// with the default applied as the catch-up applies it.
func (o RunOptions) slotLagCeiling() int64 {
	if o.Catchup.SlotLagCeiling == 0 {
		return decode.DefaultSlotLagCeiling
	}
	return o.Catchup.SlotLagCeiling
}
