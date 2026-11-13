package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/progress"
)

// Work reports the pass's counters for the progress tracker: it is the
// progress.WorkSource the verifier registers for the lifetime of Verify or
// Check. Every counter is read from memory — the pass knows what it has
// digested and recopied — so a poll never waits on the database, and the
// counters reset to zero when a pass starts.
//
//   - chunks_compared is the number of chunks the comparison has digested
//     on both sides; a digest counts when its transaction commits. Under
//     DivergenceRecopy a recopy that collided is followed by another
//     comparison, whose chunks count here too, so the figure can pass the
//     number of chunks through the watermark.
//   - rows_hashed is the number of source rows those digests covered.
//   - rows_to_hash is the number of rows the comparison expects to hash:
//     the source's catalog row count (pg_class.reltuples) read when the
//     comparison starts, the same estimate the copy reports as rows_total.
//     It is 0 for a table ANALYZE has never visited, counts the whole
//     source even when the pass stops at a partial watermark, and drifts
//     from the rows actually there as the source is written, so
//     rows_hashed can end short of it or pass it. A comparison that runs
//     again under DivergenceRecopy reads it afresh while rows_hashed keeps
//     counting.
//   - chunks_mismatched is the number of chunks the comparison found
//     differing.
//   - chunks_repaired is the number of chunks whose recopy has committed.
//     Every differing chunk is recopied in one transaction, so it moves from
//     zero to chunks_mismatched at that commit.
//   - chunks_reread is the number of chunks digested again in a fresh
//     snapshot after the comparison: under Check the repaired chunks reread
//     after the recopy, so it climbs towards chunks_repaired and the repair
//     phase's remaining work is chunks_repaired − chunks_reread; under
//     CheckLive every settling reread of a differing or repaired chunk —
//     a reread of every row of the chunk on both sides, judged key by
//     key — so it can pass chunks_mismatched while the pass waits on a
//     chunk some key of which keeps changing.
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

// rowsToHashSQL reads the catalog's last-known row count of the source: the
// figure ANALYZE or VACUUM recorded, -1 for a table neither has visited. It
// runs in the pass's guarded transaction, after the guard has confirmed the
// source is the proven relation, and every catalog name in it is qualified,
// so the figure does not depend on the transaction's search_path (CO-9).
const rowsToHashSQL = `
	SELECT COALESCE(
		(SELECT c.reltuples
		   FROM pg_catalog.pg_class c
		  WHERE c.oid OPERATOR(pg_catalog.=) $1::pg_catalog.oid),
		-1)`

// countRowsToHash records the source's catalog row count, read in tx, as
// the comparison's rows_to_hash. A negative figure means the catalog has
// none, which reports as 0 rather than as an unsigned wraparound.
func (v *Verifier) countRowsToHash(ctx context.Context, tx pgx.Tx) error {
	var relTuples float64
	if err := tx.QueryRow(ctx, rowsToHashSQL, v.shadow.SourceOID()).Scan(&relTuples); err != nil {
		return fmt.Errorf("read the row count of %s.%s: %w", v.target.Schema(), v.target.Table(), err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.work.RowsToHash = 0
	if relTuples > 0 {
		v.work.RowsToHash = uint64(relTuples)
	}
	return nil
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
