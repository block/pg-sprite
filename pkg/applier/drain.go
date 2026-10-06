package applier

import (
	"sort"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// Batch is what one Drain decided: the entries to flush now, in ascending key
// order, and how many buffered entries it discarded or left waiting. The
// flush takes two passes over Entries: it completes every entry CompleteFirst
// names, then writes. Key order does not express that dependency.
type Batch struct {
	// Entries are the landed keys' entries, each a copy the buffer no longer
	// holds; a later Add for the same key does not change it. They are in
	// ascending key order, which is not an order the flush may write them
	// in one pass: a moved image sorts by its new key, after or before the
	// delete marker at the old key it still has to read (see CompleteFirst).
	Entries []Entry
	// Discarded counts the entries dropped because their keys were uncut:
	// the copier's own read of those chunks will see the change.
	Discarded int
	// Deferred counts the entries still buffered because their keys are
	// inside an in-flight chunk, or travel with one that is. This is the
	// number a stalled catch-up is waiting on the copier for.
	Deferred int
}

// OldestFirstLSN returns the earliest FirstLSN among the batch's entries, and
// false when the batch is empty. Until the flush of this batch commits, the
// position the stream owner may confirm is bounded by this and by the
// buffer's OldestPending together, whichever is lower: the batch's entries
// have left the buffer, so OldestPending alone no longer covers them, and a
// restart after a failed flush must replay from below their first event.
func (b Batch) OldestFirstLSN() (decode.LSN, bool) {
	var oldest decode.LSN
	found := false
	for _, e := range b.Entries {
		if !found || e.FirstLSN < oldest {
			oldest = e.FirstLSN
			found = true
		}
	}
	return oldest, found
}

// CompleteFirst returns the images whose write depends on a shadow row that
// another entry in the same batch deletes: a moved image still carrying an
// unchanged-TOAST marker. Its value lives in the shadow row under OldKey
// (D13), and the delete marker at that old key is in the batch too, sorted by
// its own key, so a flush that wrote Entries in order could delete the row
// before reading it. The flush completes these images first, then writes. An
// image that did not move is not named: its marker stands for the row under
// its own key, which no other entry deletes — the primary path's upsert
// writes only present columns and leaves that value in place, and the
// fallback, which needs every image whole, completes it from that row before
// the fallback's own deletes. The result shares Entries' order.
func (b Batch) CompleteFirst() []Entry {
	var first []Entry
	for _, e := range b.Entries {
		if e.Kind == Image && e.OldKey != nil && e.HasMarker() {
			first = append(first, e)
		}
	}
	return first
}

// Drain decides every buffered entry against one snapshot of the copier's
// position. Each key is judged on its own (CO-4): an uncut key's entry is
// discarded, because the copier's own read of that chunk will see the change;
// a landed key's entry is flushed; an in-flight key's entry stays buffered
// until a later Drain finds its chunk landed. Discarding is sound only when
// the copier reads an uncut key's chunk after the buffered change is visible,
// so pos must be read after the last Add: the caller adds every event it
// holds, then reads the copier's Position, then drains. Discarded and flushed
// entries leave the buffer; a flush that then fails is not re-buffered — the
// stream replays from its last confirmed position, which stays below
// OldestPending and the batch's OldestFirstLSN until the flush commits, and
// rebuilds them.
//
// An image that moved from an old key, and the entry still buffered at that
// old key, travel together once neither is uncut: if either is in flight both
// wait, otherwise both flush. The flush completes the image's unchanged-TOAST
// markers before it deletes anything (D13), and that reading must not race the
// deletion or copy of the old key's row; Batch.CompleteFirst names the images
// it applies to.
func (b *Buffer) Drain(pos copier.Position) Batch {
	// INV: CO-4
	state := make(map[int64]copier.KeyState, len(b.entries))
	for key := range b.entries {
		state[key] = pos.Classify(key)
	}
	b.deferPairs(state)
	var batch Batch
	b.hasOldest = false
	for key, e := range b.entries {
		switch state[key] {
		case copier.KeyUncut:
			delete(b.entries, key)
			batch.Discarded++
		case copier.KeyLanded:
			delete(b.entries, key)
			batch.Entries = append(batch.Entries, *e)
		case copier.KeyInFlight:
			batch.Deferred++
			b.trackOldest(e.FirstLSN)
		}
	}
	// Key order makes the batch deterministic for callers and tests. It is not
	// a write order: a moved image's completion read of the old key's row must
	// precede the delete marker at that key, and the two sort independently.
	// CompleteFirst carries that dependency.
	sort.Slice(batch.Entries, func(i, j int) bool { return batch.Entries[i].Key < batch.Entries[j].Key })
	return batch
}

// movePair is an image that moved from an old key whose entry is also
// buffered: the two halves of a move that Drain judges together.
type movePair struct {
	image, old int64
}

// deferPairs marks both halves of every travelling pair in flight when either
// half is. Two images may share one old key, so a deferral can spread through
// that key to another pair; the passes repeat until one changes nothing.
// Every pass that continues moves at least one key to in flight, so the work
// is bounded by the number of pairs squared, not by the buffer: the pairs are
// collected once, and a buffer with no moves costs one scan.
func (b *Buffer) deferPairs(state map[int64]copier.KeyState) {
	var pairs []movePair
	for key, e := range b.entries {
		if e.OldKey == nil {
			continue
		}
		if _, paired := b.entries[*e.OldKey]; paired {
			pairs = append(pairs, movePair{image: key, old: *e.OldKey})
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range pairs {
			if !waitsTogether(state[p.image], state[p.old]) {
				continue
			}
			if state[p.image] != copier.KeyInFlight || state[p.old] != copier.KeyInFlight {
				state[p.image] = copier.KeyInFlight
				state[p.old] = copier.KeyInFlight
				changed = true
			}
		}
	}
}

// waitsTogether reports whether a moved image and its old key's entry must
// both be deferred: neither is uncut, and at least one is in flight.
func waitsTogether(image, old copier.KeyState) bool {
	if image == copier.KeyUncut || old == copier.KeyUncut {
		return false
	}
	return image == copier.KeyInFlight || old == copier.KeyInFlight
}
