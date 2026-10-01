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
//   - chunks_compared is the number of two-sided chunk digests the pass has
//     completed: every chunk of the comparison once, and every repaired
//     chunk once more when its reread completes.
//   - rows_hashed is the number of source rows those digests covered, summed
//     the same way.
//   - chunks_mismatched is the number of chunks the comparison found
//     differing.
//   - chunks_repaired is the number of chunks whose recopy has committed.
//     Every differing chunk is recopied in one transaction, so it moves from
//     zero to chunks_mismatched at that commit.
func (v *Verifier) Work(context.Context) (progress.Work, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.work, nil
}

// report resets the counters for a new pass and registers the verifier with
// the tracker, when there is one. The returned stop is the fence before the
// pass returns: it waits for an in-flight poll, so no poll that began while
// the pass ran completes against a verifier whose caller has moved on.
func (v *Verifier) report() (stop func()) {
	v.mu.Lock()
	v.work = progress.Work{}
	v.mu.Unlock()
	if v.opts.Tracker != nil {
		v.opts.Tracker.SetWorkSource(v)
	}
	return func() {
		if v.opts.Tracker != nil {
			v.opts.Tracker.StopWorkSource()
		}
	}
}

// countCompared records one completed two-sided digest covering rows
// source rows.
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
