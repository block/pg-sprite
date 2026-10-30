package copier

import (
	"math"
	"slices"
)

// ledger is the copier's bookkeeping of which chunks are claimed and which
// have landed. It knows nothing about the database or about goroutines: the
// copier mutates it under one lock and snapshots it into a Position. Chunks
// are claimed in key order but land in any order, so the watermark advances
// only over the contiguous prefix of landed chunks, while the cut frontier
// follows claims.
type ledger struct {
	cut       Watermark
	watermark Watermark
	// next is the lower bound of the chunk that will extend the watermark;
	// meaningful while !complete.
	next     int64
	complete bool

	// inFlight holds claimed chunks that have not landed, in key order: a
	// chunk is claimed only at the frontier, so claim order is key order.
	inFlight []Chunk
	// landed holds chunks that committed above the watermark and wait for
	// the chunks below them.
	landed []Chunk
	// partial holds, for an in-flight chunk that has committed part of
	// itself, the rows it has landed so far and the keys it has yet to land.
	partial map[Chunk]partialChunk
	// rows counts every row a committed pass inserted, the partial chunks'
	// included.
	rows int64
}

// partialChunk is what an in-flight chunk has committed so far and what it
// still owes: pending is never empty, since a chunk with nothing pending
// lands.
type partialChunk struct {
	rows    int64
	pending []int64
}

// newLedger resumes bookkeeping after from: every key at or below it counts
// as landed, and the first chunk to extend the watermark starts just above it.
func newLedger(from Watermark) *ledger {
	l := &ledger{cut: from, watermark: from, partial: make(map[Chunk]partialChunk)}
	l.next, l.complete = startAfter(from)
	return l
}

// claim registers a chunk a worker is about to read and moves the cut
// frontier to its upper bound. It reports false, registering nothing, for a
// chunk that does not start just above the frontier: the frontier is the
// applier's discard boundary, and a chunk registered ahead of an unregistered
// one would make the gap between them read as landed.
func (l *ledger) claim(chunk Chunk) bool {
	// INV: CO-4
	if !l.startsAtFrontier(chunk) {
		return false
	}
	l.inFlight = append(l.inFlight, chunk)
	l.cut = NewWatermark(chunk.upper)
	return true
}

// startsAtFrontier reports whether chunk is the next consecutive chunk: the
// first chunk of the key space while nothing is cut, and otherwise the chunk
// whose lower bound is one past the frontier.
func (l *ledger) startsAtFrontier(chunk Chunk) bool {
	if !l.cut.Valid() {
		return chunk.lower == math.MinInt64
	}
	if l.cut.Value() == math.MaxInt64 {
		return false
	}
	return chunk.lower == l.cut.Value()+1
}

// settle records that a claimed chunk has committed rows rows in all so far
// and still owes the rows under pending, which must be non-empty and
// ascending. The chunk stays in flight — the watermark cannot pass it, and a
// resumed run clears and re-copies it — but a Position now classifies its
// keys outside pending as landed, so the applier writes them while the
// copier waits for the rows holding the pending keys' values to be
// rewritten (CO-4). It reports false, recording nothing, for a chunk that
// is not in flight, and for an empty pending: a chunk owing nothing lands.
func (l *ledger) settle(chunk Chunk, rows int64, pending []int64) bool {
	if !slices.Contains(l.inFlight, chunk) {
		return false
	}
	if len(pending) == 0 {
		return false
	}
	l.rows += rows - l.partial[chunk].rows
	l.partial[chunk] = partialChunk{rows: rows, pending: slices.Clone(pending)}
	return true
}

// land records that a claimed chunk committed rows rows in all — the rows
// of every settled pass included — and advances the watermark over every
// landed chunk now contiguous with it. It reports false when the chunk was
// not in flight. A chunk whose transaction did not commit is never landed:
// it stays in flight, so the applier keeps deferring for its keys and the
// watermark cannot pass it.
func (l *ledger) land(chunk Chunk, rows int64) bool {
	i := slices.Index(l.inFlight, chunk)
	if i < 0 {
		return false
	}
	l.inFlight = slices.Delete(l.inFlight, i, i+1)
	l.rows += rows - l.partial[chunk].rows
	delete(l.partial, chunk)
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

// position snapshots the ledger. The in-flight chunks and the pending keys
// are copied so the caller can read them without the copier's lock; the
// pending entries follow the in-flight order, so they are ascending too.
func (l *ledger) position() Position {
	pos := Position{
		Watermark:    l.watermark,
		Cut:          l.cut,
		InFlight:     slices.Clone(l.inFlight),
		RowsInserted: l.rows,
	}
	for _, chunk := range l.inFlight {
		if partial, ok := l.partial[chunk]; ok {
			pos.Pending = append(pos.Pending, PendingKeys{Chunk: chunk, Keys: slices.Clone(partial.pending)})
		}
	}
	return pos
}
