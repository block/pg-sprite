package applier

import (
	"sort"

	"github.com/block/pg-sprite/pkg/copier"
)

// Drain decides every buffered entry against one snapshot of the copier's
// position and returns the entries to flush now, in ascending key order.
// Each key is judged on its own (CO-4): an uncut key's entry is discarded,
// because the copier's own read of that chunk will see the change; a landed
// key's entry is flushed; an in-flight key's entry stays buffered until a
// later Drain finds its chunk landed. Discarded and flushed entries leave the
// buffer; a flush that then fails is not re-buffered — the stream replays from
// its last confirmed position, below OldestPending, and rebuilds them.
//
// An image that moved from an old key, and the entry still buffered at that
// old key, travel together once neither is uncut: if either is in flight both
// wait, otherwise both flush. The flush completes the image's unchanged-TOAST
// markers before it deletes anything (D13), and that reading must not race the
// deletion or copy of the old key's row.
func (b *Buffer) Drain(pos copier.Position) (flush []Entry, discarded int) {
	// INV: CO-4
	state := make(map[int64]copier.KeyState, len(b.entries))
	for key := range b.entries {
		state[key] = pos.Classify(key)
	}
	b.deferPairs(state)
	for key, e := range b.entries {
		switch state[key] {
		case copier.KeyUncut:
			delete(b.entries, key)
			discarded++
		case copier.KeyLanded:
			delete(b.entries, key)
			flush = append(flush, *e)
		case copier.KeyInFlight:
			// Deferred: stays buffered for a later Drain.
		}
	}
	sort.Slice(flush, func(i, j int) bool { return flush[i].Key < flush[j].Key })
	return flush, discarded
}

// deferPairs marks both halves of every travelling pair in flight when either
// half is. Two images may share one old key, so a deferral can spread through
// that key to another pair; the loop repeats until a pass changes nothing,
// and every pass that continues moves at least one key to in flight, so it
// ends within len(state) passes.
func (b *Buffer) deferPairs(state map[int64]copier.KeyState) {
	for changed := true; changed; {
		changed = false
		for key, e := range b.entries {
			if e.OldKey == nil {
				continue
			}
			old := *e.OldKey
			if _, paired := b.entries[old]; !paired {
				continue
			}
			if !waitsTogether(state[key], state[old]) {
				continue
			}
			if state[key] != copier.KeyInFlight || state[old] != copier.KeyInFlight {
				state[key] = copier.KeyInFlight
				state[old] = copier.KeyInFlight
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
