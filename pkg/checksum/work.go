package checksum

import (
	"context"

	"github.com/block/pg-sprite/pkg/progress"
)

// Work reports the pass's counters for the progress tracker: it is the
// progress.WorkSource the verifier registers for the lifetime of Verify or
// Check. Every counter is read from memory — the pass knows what it has
// digested and recopied — so a poll never waits on the database, and the
// counters reset to zero when a pass starts.
//
//   - chunks_compared is the number of chunks the comparison has digested
//     on both sides; a digest counts when its transaction commits.
//   - rows_hashed is the number of source rows those digests covered.
//   - chunks_mismatched is the number of chunks the comparison found
//     differing.
//   - chunks_repaired is the number of chunks whose recopy has committed.
//     Every differing chunk is recopied in one transaction, so it moves from
//     zero to chunks_mismatched at that commit.
//   - chunks_reread is the number of repaired chunks digested again after
//     the recopy; it climbs towards chunks_repaired, so the repair phase's
//     remaining work is chunks_repaired − chunks_reread.
//
// The counters are the pass's, not the step's: once the pass returns the
// tracker no longer asks the verifier, and the pass's final figures are the
// Report or Outcome it returned.
func (v *Verifier) Work(context.Context) (progress.Work, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.work, nil
}

// report starts a pass: it resets the counters and registers the verifier
// with the tracker, when there is one. It refuses to start a pass while
// another runs on this verifier, since the second would reset the first's
// counters and the first's stop would end the second's registration. The
// returned stop is the fence before the pass returns: it waits for an
// in-flight poll, so no poll that began while the pass ran completes
// against a verifier whose caller has moved on, and then lets the next pass
// start.
func (v *Verifier) report() (stop func(), err error) {
	v.mu.Lock()
	if v.running {
		v.mu.Unlock()
		return nil, ErrPassRunning
	}
	v.running = true
	v.work = progress.Work{}
	v.mu.Unlock()
	if v.opts.Tracker != nil {
		v.opts.Tracker.SetWorkSource(v)
	}
	return func() {
		if v.opts.Tracker != nil {
			v.opts.Tracker.StopWorkSource()
		}
		v.mu.Lock()
		v.running = false
		v.mu.Unlock()
	}, nil
}

// countCompared records one committed two-sided digest of the comparison
// covering rows source rows.
func (v *Verifier) countCompared(rows int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.work.ChunksCompared++
	v.work.RowsHashed += uint64(rows)
}

// countMismatch records one chunk the comparison found differing.
func (v *Verifier) countMismatch() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.work.ChunksMismatched++
}

// countRepaired records chunks whose recopy has committed.
func (v *Verifier) countRepaired(chunks int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.work.ChunksRepaired += uint64(chunks)
}

// countReread records one repaired chunk whose reread digest has committed.
func (v *Verifier) countReread() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.work.ChunksReread++
}
