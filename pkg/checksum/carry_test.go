package checksum

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/copier"
)

// stillChanging is the chunk a live pass gave up on with keys left
// changing at its last reread.
func stillChanging(keys ...int64) *UnresolvedError {
	return &UnresolvedError{Cause: CauseHot, Keys: len(keys), hot: keys}
}

// The keys every given-up chunk left changing are collected in the order
// the chunks settled, which is key order, and can be left for the swap
// while they number no more than the limit: exactly the limit is carried.
func TestHotKeysCollectTheKeysEachChunkLeftChanging(t *testing.T) {
	h := hotKeys{limit: 3}
	h.record(stillChanging(7))
	h.record(stillChanging(1200, 1900))

	assert.True(t, h.carriable())
	assert.Equal(t, []int64{7, 1200, 1900}, h.keys)
}

// One key past the limit and the pass leaves nothing for the swap: the
// keys are dropped, and a later chunk small enough to fit on its own does
// not bring the pass back under the limit.
func TestHotKeysPastTheLimitAreNotCarried(t *testing.T) {
	h := hotKeys{limit: 3}
	h.record(stillChanging(7, 8))
	h.record(stillChanging(1200, 1900))
	assert.False(t, h.carriable(), "four keys against a limit of three")
	assert.Empty(t, h.keys)

	h.record(stillChanging(2500))
	assert.False(t, h.carriable(), "the pass stays over its limit")
	assert.Empty(t, h.keys)
}

// A limit of zero is a pass that may leave nothing: one changing key is
// already past it.
func TestHotKeysWithNoLimitCarryNothing(t *testing.T) {
	var h hotKeys
	h.record(stillChanging(7))

	assert.False(t, h.carriable())
}

// A chunk the catch-up never confirmed past names no key, because none of
// its keys was judged. The swap comparing other chunks' keys would leave
// that whole chunk unproven, so one such chunk keeps the pass from being
// carried however few keys the others left.
func TestHotKeysDoNotCarryAPassWithAChunkThatNamedNoKey(t *testing.T) {
	h := hotKeys{limit: 3}
	h.record(stillChanging(7))
	h.record(&UnresolvedError{Cause: CauseLagging})

	assert.False(t, h.carriable())
}

// A pass that gave up on no chunk has nothing to leave.
func TestHotKeysCarryNothingWhenEveryChunkSettled(t *testing.T) {
	h := hotKeys{limit: 3}

	assert.False(t, h.carriable())
}

// The keys are handed to the swap only by a pass that can mint the proof
// the swap acts on: one through the complete watermark with no confirmed
// divergence. A pass through a prefix proves only that prefix, and a pass
// that confirmed a divergence has a finding to report instead.
func TestSettledChunksCarryKeysOnlyForACompletePassWithNoDivergence(t *testing.T) {
	complete := copier.NewWatermark(math.MaxInt64)
	left := hotKeys{limit: 3, keys: []int64{7}}

	assert.Equal(t, []int64{7}, settledChunks{unsettled: 1, hot: left}.carried(complete))
	assert.Nil(t, settledChunks{unsettled: 1, hot: left}.carried(copier.NewWatermark(5000)), "a pass through key 5000 only")
	assert.Nil(t, settledChunks{unsettled: 1, hot: left, mismatches: []Mismatch{{}}}.carried(complete), "a divergence confirmed alongside")
}

// The keys a proof names are the caller's to keep: sorting or editing the
// slice Carried returns leaves the proof's own keys as they were.
func TestCarriedIsTheCallersToKeep(t *testing.T) {
	proof := VerifiedShadow{carried: []int64{7, 9}}

	got := proof.Carried()
	got[0] = 99

	assert.Equal(t, []int64{7, 9}, proof.Carried())
}
