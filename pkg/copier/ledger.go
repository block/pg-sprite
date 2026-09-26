package copier

import (
	"cmp"
	"math"
	"slices"
)

// ledger is the copier's bookkeeping of which chunks are claimed and which
// have landed. It knows nothing about the database or about goroutines: the
// copier mutates it under one lock and snapshots it into a Position. Chunks
// arrive in claim order but land in any order, so the watermark advances only
// over the contiguous prefix of landed chunks, while the cut frontier follows
// claims.
type ledger struct {
	cut      int64
	cutValid bool

	watermark Watermark
	// next is the lower bound of the chunk that will extend the watermark;
	// meaningful while !complete.
	next     int64
	complete bool

	inFlight []Chunk
	// landed holds chunks that committed above the watermark and wait for
	// the chunks below them.
	landed []Chunk
	rows   int64
}

// newLedger resumes bookkeeping after from: every key at or below it counts
// as landed, and the first chunk to extend the watermark starts just above it.
func newLedger(from Watermark) *ledger {
	l := &ledger{watermark: from}
	l.next, l.complete = startAfter(from)
	if from.Valid() {
		l.cut, l.cutValid = from.Value(), true
	}
	return l
}

// claim registers a chunk a worker is about to read and moves the cut
// frontier to its upper bound.
func (l *ledger) claim(chunk Chunk) {
	l.inFlight = append(l.inFlight, chunk)
	// INV: CO-4
	if !l.cutValid || chunk.upper > l.cut {
		l.cut, l.cutValid = chunk.upper, true
	}
}

// release forgets a claimed chunk whose transaction did not commit. It
// reports false when the chunk was not in flight.
func (l *ledger) release(chunk Chunk) bool {
	i := slices.Index(l.inFlight, chunk)
	if i < 0 {
		return false
	}
	l.inFlight = slices.Delete(l.inFlight, i, i+1)
	return true
}

// land records that a claimed chunk committed rows rows and advances the
// watermark over every landed chunk now contiguous with it. It reports false
// when the chunk was not in flight.
func (l *ledger) land(chunk Chunk, rows int64) bool {
	if !l.release(chunk) {
		return false
	}
	l.rows += rows
	l.landed = append(l.landed, chunk)
	l.advance()
	return true
}

// advance moves the watermark up while the chunk that starts just above it
// has landed. Chunks are consecutive, so contiguity is an exact match on the
// lower bound.
func (l *ledger) advance() {
	for !l.complete {
		i := slices.IndexFunc(l.landed, func(c Chunk) bool { return c.lower == l.next })
		if i < 0 {
			return
		}
		chunk := l.landed[i]
		l.landed = slices.Delete(l.landed, i, i+1)
		// INV: CO-4
		l.watermark = NewWatermark(chunk.upper)
		if chunk.upper == math.MaxInt64 {
			l.complete = true
			return
		}
		l.next = chunk.upper + 1
	}
}

// position snapshots the ledger. The in-flight chunks are copied and sorted
// so the caller can read them without the copier's lock.
func (l *ledger) position() Position {
	inFlight := slices.Clone(l.inFlight)
	slices.SortFunc(inFlight, func(a, b Chunk) int { return cmp.Compare(a.lower, b.lower) })
	return Position{
		Watermark:    l.watermark,
		Cut:          l.cut,
		CutValid:     l.cutValid,
		InFlight:     inFlight,
		RowsInserted: l.rows,
	}
}
