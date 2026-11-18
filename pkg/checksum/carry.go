package checksum

import "github.com/block/pg-sprite/pkg/copier"

// MaxCarryKeys is the most keys a live pass may be told to leave for the
// swap (LiveOptions.CarryKeys). The swap compares every carried key on
// both sides while it holds ACCESS EXCLUSIVE on the source, so the bound
// on the keys is a bound on what the swap reads under that lock.
const MaxCarryKeys = 1024

// hotKeys collects the keys a live pass could not judge because they kept
// changing, across every chunk that ran out of rereads, and decides
// whether the swap can compare them in the pass's place. It holds no more
// than limit keys: a pass that leaves more is not carried, and the keys
// are dropped as soon as that is known.
type hotKeys struct {
	// limit is the most keys the pass may leave for the swap; zero leaves
	// none.
	limit int
	// keys are the keys still changing, in ascending order: chunks settle
	// in key order and each names its keys in key order.
	keys []int64
	// exceeded is set once the chunks named more than limit keys.
	exceeded bool
	// unnamed counts the chunks left unsettled that named no key: the
	// catch-up never confirmed past their read, so no key of them was
	// judged and the whole chunk is unknown.
	unnamed int
}

// record adds the keys one unsettled chunk left changing.
func (h *hotKeys) record(unresolved *UnresolvedError) {
	if len(unresolved.hot) == 0 {
		h.unnamed++
		return
	}
	if h.exceeded {
		return
	}
	if len(h.keys)+len(unresolved.hot) > h.limit {
		h.exceeded = true
		h.keys = nil
		return
	}
	h.keys = append(h.keys, unresolved.hot...)
}

// carriable reports whether every chunk the pass left unsettled is
// accounted for by a key the swap can compare: some keys were left
// changing, no more than the limit, and no chunk went unjudged as a whole.
func (h hotKeys) carriable() bool {
	if h.exceeded {
		return false
	}
	if h.unnamed > 0 {
		return false
	}
	return len(h.keys) > 0
}

// carried returns the keys the pass leaves for the swap to compare, or nil
// when it leaves none: only a pass through the complete watermark that
// confirmed no divergence can hand its unjudged keys on, since any other
// pass mints no proof for the swap to act on.
func (s settledChunks) carried(through copier.Watermark) []int64 {
	if !through.Complete() {
		return nil
	}
	if len(s.mismatches) > 0 {
		return nil
	}
	if !s.hot.carriable() {
		return nil
	}
	return s.hot.keys
}

// proveCarrying mints the proof of a live pass that judged every key of
// the table but the carried ones: the verified shadow, naming them, and no
// clean watermark, since the pass did not read every chunk clean. The
// shadow equals the source for every other key as of the positions the
// pass settled against; the carried keys are the swap's to compare.
func (v *Verifier) proveCarrying(report Report, carried []int64) Outcome {
	// INV: CO-1, CO-2
	verified := newVerifiedShadow(v.shadow, report.Through, v.opts.Clock.Now(), true)
	verified.carried = carried
	return Outcome{Report: report, verified: verified}
}
