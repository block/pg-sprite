package copier

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustChunk(t *testing.T, lower, upper int64) Chunk {
	t.Helper()
	chunk, err := NewChunk(lower, upper)
	require.NoError(t, err)
	return chunk
}

// Chunks are claimed in key order and land in any order. The cut frontier
// follows claims, the watermark follows only the contiguous landed prefix,
// and a Position shows the in-flight chunks between them.
func TestLedgerWatermarkFollowsTheContiguousPrefix(t *testing.T) {
	l := newLedger(Watermark{})
	fresh := l.position()
	assert.False(t, fresh.CutValid, "nothing claimed, nothing cut")
	assert.False(t, fresh.Watermark.Valid())
	assert.Empty(t, fresh.InFlight)

	first := mustChunk(t, math.MinInt64, 10)
	second := mustChunk(t, 11, 20)
	last := mustChunk(t, 21, math.MaxInt64)
	require.True(t, l.claim(first))
	require.True(t, l.claim(second))
	require.True(t, l.claim(last))
	claimed := l.position()
	assert.True(t, claimed.CutValid)
	assert.Equal(t, int64(math.MaxInt64), claimed.Cut, "the frontier is the highest claimed key")
	assert.Equal(t, []Chunk{first, second, last}, claimed.InFlight)

	require.True(t, l.land(second, 10))
	afterSecond := l.position()
	assert.False(t, afterSecond.Watermark.Valid(), "a landed chunk above an unlanded one does not move the watermark")
	assert.Equal(t, []Chunk{first, last}, afterSecond.InFlight)
	assert.Equal(t, int64(10), afterSecond.RowsInserted)

	require.True(t, l.land(first, 3))
	afterFirst := l.position()
	assert.Equal(t, NewWatermark(20), afterFirst.Watermark, "the watermark jumps over the chunk that had already landed")
	assert.Equal(t, []Chunk{last}, afterFirst.InFlight)
	assert.Equal(t, int64(13), afterFirst.RowsInserted)

	require.True(t, l.land(last, 0))
	done := l.position()
	assert.Equal(t, NewWatermark(math.MaxInt64), done.Watermark, "the open-above chunk completes the copy")
	assert.Empty(t, done.InFlight)
	assert.True(t, l.complete)
}

// A claim is accepted only at the frontier. Registering a chunk ahead of an
// unregistered one would leave the gap between them reading as landed, so
// the ledger refuses it and its state is untouched; the same goes for a
// chunk below the frontier and for any chunk once the key space is cut.
func TestLedgerClaimsOnlyAtTheFrontier(t *testing.T) {
	first := mustChunk(t, math.MinInt64, 10)
	second := mustChunk(t, 11, 20)
	last := mustChunk(t, 21, math.MaxInt64)

	l := newLedger(Watermark{})
	assert.False(t, l.claim(second), "the first claim must be the chunk open below")
	assert.False(t, l.position().CutValid, "a refused claim cuts nothing")
	require.True(t, l.claim(first))
	assert.False(t, l.claim(last), "a chunk past the frontier is refused")
	assert.False(t, l.claim(first), "a chunk at or below the frontier is refused")
	assert.Equal(t, []Chunk{first}, l.position().InFlight)
	assert.Equal(t, int64(10), l.position().Cut)
	require.True(t, l.claim(second))
	require.True(t, l.claim(last))
	assert.False(t, l.claim(mustChunk(t, 30, 40)), "nothing can be claimed once the key space is cut")

	resumed := newLedger(NewWatermark(100))
	assert.False(t, resumed.claim(mustChunk(t, 100, 200)), "a resumed ledger's first chunk starts just above the watermark")
	assert.False(t, resumed.claim(mustChunk(t, 102, 200)))
	require.True(t, resumed.claim(mustChunk(t, 101, 200)))
}

// A chunk whose transaction did not commit stays in flight: the frontier
// stays where the claim put it, a Position keeps classifying its keys as
// in flight so the applier keeps deferring changes for them, and the
// watermark cannot pass it.
func TestLedgerUnlandedChunkStaysInFlight(t *testing.T) {
	l := newLedger(Watermark{})
	first := mustChunk(t, math.MinInt64, 10)
	second := mustChunk(t, 11, 20)
	require.True(t, l.claim(first))
	require.True(t, l.claim(second))

	require.True(t, l.land(second, 10))
	pos := l.position()
	assert.Equal(t, []Chunk{first}, pos.InFlight)
	assert.Equal(t, int64(20), pos.Cut)
	assert.False(t, pos.Watermark.Valid(), "the watermark waits for the unlanded chunk")
	assert.Equal(t, KeyInFlight, pos.Classify(5), "a key in the unlanded chunk is deferred, not landed")
	assert.Equal(t, KeyLanded, pos.Classify(15), "a key in the chunk that landed above it is landed")
	assert.Equal(t, KeyUncut, pos.Classify(21))

	assert.False(t, l.land(second, 1), "a chunk lands at most once")
	assert.False(t, l.land(mustChunk(t, 21, 30), 1), "an unclaimed chunk cannot land")
	assert.Equal(t, int64(10), l.position().RowsInserted, "a refused landing counts no rows")
}

// Resuming after a watermark treats every key at or below it as landed and
// continues the contiguous prefix from just above it.
func TestLedgerResumesAfterTheWatermark(t *testing.T) {
	l := newLedger(NewWatermark(100))
	resumed := l.position()
	assert.Equal(t, NewWatermark(100), resumed.Watermark)
	assert.True(t, resumed.CutValid)
	assert.Equal(t, int64(100), resumed.Cut, "keys at or below the resume watermark are landed, not uncut")

	next := mustChunk(t, 101, 200)
	require.True(t, l.claim(next))
	require.True(t, l.land(next, 100))
	assert.Equal(t, NewWatermark(200), l.position().Watermark)

	complete := newLedger(NewWatermark(math.MaxInt64))
	assert.True(t, complete.complete, "a watermark at the largest key leaves nothing to copy")
	assert.Equal(t, NewWatermark(math.MaxInt64), complete.position().Watermark)
}
