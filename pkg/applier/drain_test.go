package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// position is a copier snapshot with landed keys at or below cut except for
// the in-flight ranges.
func position(t *testing.T, cut int64, inFlight ...[2]int64) copier.Position {
	t.Helper()
	pos := copier.Position{Cut: copier.NewWatermark(cut)}
	for _, r := range inFlight {
		chunk, err := copier.NewChunk(r[0], r[1])
		require.NoError(t, err)
		pos.InFlight = append(pos.InFlight, chunk)
	}
	return pos
}

func keys(entries []Entry) []int64 {
	out := make([]int64, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return out
}

// The CO-4 three-way rule, one key per state: landed flushes, uncut is
// discarded, in flight stays buffered; the flush is in key order whatever
// the arrival order.
func TestDrainJudgesEachKeyAgainstThePosition(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 30, col("label", "landed-high"))))
	require.NoError(t, b.Add(update(20, 15, col("label", "in-flight"))))
	require.NoError(t, b.Add(update(30, 5, col("label", "landed-low"))))
	require.NoError(t, b.Add(del(40, 40)))

	flush, discarded := b.Drain(position(t, 30, [2]int64{11, 20}))

	assert.Equal(t, []int64{5, 30}, keys(flush))
	assert.Equal(t, 1, discarded, "key 40 is above the cut frontier")
	assert.Equal(t, 1, b.Len(), "key 15 waits for its chunk")
	oldest, ok := b.OldestPending()
	require.True(t, ok)
	assert.Equal(t, decode.LSN(20), oldest, "the confirmed position may not pass the deferred entry")

	flush, discarded = b.Drain(position(t, 30))
	assert.Equal(t, []int64{15}, keys(flush))
	assert.Equal(t, 0, discarded)
	assert.Equal(t, 0, b.Len())
}

// Before the copier claims anything every key is uncut: the whole buffer is
// discarded, since the copy will read every row.
func TestDrainDiscardsEverythingBeforeTheFirstClaim(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 1, col("label", "a"))))
	require.NoError(t, b.Add(del(20, 2)))
	flush, discarded := b.Drain(copier.Position{})
	assert.Empty(t, flush)
	assert.Equal(t, 2, discarded)
	assert.Equal(t, 0, b.Len())
}

// A deferred entry keeps merging later events until its chunk lands, so the
// flush that finally runs carries the newest image.
func TestDrainDeferredEntryKeepsMerging(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 15, col("label", "first"), col("doc", "d"))))
	_, _ = b.Drain(position(t, 30, [2]int64{11, 20}))
	require.NoError(t, b.Add(update(20, 15, col("label", "second"), marker("doc"))))

	flush, _ := b.Drain(position(t, 30))
	require.Len(t, flush, 1)
	assert.Equal(t, []decode.Column{col("label", "second"), col("doc", "d")}, flush[0].Columns)
}

// Drain returns the entry by value: what the caller flushes is not changed by
// a later Add for the same key.
func TestDrainReturnsCopies(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 1, col("label", "a"))))
	flush, _ := b.Drain(position(t, 30))
	require.NoError(t, b.Add(update(20, 1, col("label", "b"))))
	assert.Equal(t, "a", flush[0].Columns[0].Value)
}

// A key-moving UPDATE is judged per key (D4): the deletion below the frontier
// is flushed even when the new key above it is discarded.
func TestDrainKeyMoveStraddlingTheFrontier(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 5, 5000, col("label", "a"))))

	flush, discarded := b.Drain(position(t, 1000))

	require.Len(t, flush, 1)
	assert.Equal(t, int64(5), flush[0].Key)
	assert.Equal(t, DeleteMarker, flush[0].Kind)
	assert.Equal(t, 1, discarded, "the image at 5000 is uncut; the copier will read the moved row")
	assert.Equal(t, 0, b.Len())
}

// The two halves of a move travel together once neither is uncut: a landed
// image waits for the in-flight old key, and a landed old key waits for the
// in-flight image. Neither flushes alone.
func TestDrainKeyMovePairWaitsTogether(t *testing.T) {
	cases := map[string]struct {
		from, to int64
	}{
		"old key in flight, image landed": {from: 15, to: 25},
		"old key landed, image in flight": {from: 25, to: 15},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := NewBuffer()
			require.NoError(t, b.Add(keyMove(10, tc.from, tc.to, col("label", "a"), marker("doc"))))

			flush, discarded := b.Drain(position(t, 30, [2]int64{11, 20}))
			assert.Empty(t, flush)
			assert.Equal(t, 0, discarded)
			assert.Equal(t, 2, b.Len())

			flush, _ = b.Drain(position(t, 30))
			assert.Equal(t, sortedPair(tc.from, tc.to), keys(flush), "both halves flush in one batch once the chunk lands")
		})
	}
}

// An uncut half is discarded on its own and does not hold the other back: a
// landed image whose old key is uncut flushes now (its old row will never be
// copied, so there is nothing to wait for), and an in-flight old key is not
// pulled forward by an uncut image.
func TestDrainKeyMoveUncutHalfIsJudgedAlone(t *testing.T) {
	t.Run("old key uncut, image landed", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 5000, 25, col("label", "a"), marker("doc"))))
		flush, discarded := b.Drain(position(t, 30, [2]int64{11, 20}))
		assert.Equal(t, []int64{25}, keys(flush))
		assert.Equal(t, 1, discarded)
		assert.Equal(t, 0, b.Len())
	})
	t.Run("old key in flight, image uncut", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 15, 5000, col("label", "a"))))
		flush, discarded := b.Drain(position(t, 30, [2]int64{11, 20}))
		assert.Empty(t, flush)
		assert.Equal(t, 1, discarded)
		assert.Equal(t, 1, b.Len(), "the marker at 15 waits for its own chunk")
	})
}

// A deferral spreads through a shared old key: two images that both left key
// 25 wait when either of them is in flight, even though 25 itself has landed.
func TestDrainDeferralSpreadsThroughSharedOldKey(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 25, 15, col("label", "first"))))
	require.NoError(t, b.Add(insert(20, 25, col("label", "reinserted"))))
	require.NoError(t, b.Add(keyMove(30, 25, 28, col("label", "second"))))

	flush, discarded := b.Drain(position(t, 30, [2]int64{11, 20}))
	assert.Empty(t, flush, "15 is in flight, so 25 waits, so 28 waits")
	assert.Equal(t, 0, discarded)
	assert.Equal(t, 3, b.Len())
}

// The link is to the entry buffered at the old key, whatever its kind: a row
// re-inserted at the old key travels with the image that left it.
func TestDrainKeyMovePairsWithReinsertedOldKey(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 15, 25, col("label", "moved"))))
	require.NoError(t, b.Add(insert(20, 15, col("label", "reinserted"))))

	flush, _ := b.Drain(position(t, 30, [2]int64{11, 20}))
	assert.Empty(t, flush, "the landed image at 25 waits for the in-flight re-inserted row at 15")
	assert.Equal(t, 2, b.Len())
}

func sortedPair(a, b int64) []int64 {
	if a < b {
		return []int64{a, b}
	}
	return []int64{b, a}
}
