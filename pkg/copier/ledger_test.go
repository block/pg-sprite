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

// Chunks land out of order. The cut frontier follows claims, the watermark
// follows only the contiguous landed prefix, and a Position shows the
// in-flight chunks between them in key order whatever order they were
// claimed in.
func TestLedgerWatermarkFollowsTheContiguousPrefix(t *testing.T) {
	l := newLedger(Watermark{})
	fresh := l.position()
	assert.False(t, fresh.CutValid, "nothing claimed, nothing cut")
	assert.False(t, fresh.Watermark.Valid())
	assert.Empty(t, fresh.InFlight)

	first := mustChunk(t, math.MinInt64, 10)
	second := mustChunk(t, 11, 20)
	last := mustChunk(t, 21, math.MaxInt64)
	l.claim(second)
	l.claim(first)
	l.claim(last)
	claimed := l.position()
	assert.True(t, claimed.CutValid)
	assert.Equal(t, int64(math.MaxInt64), claimed.Cut, "the frontier is the highest claimed key")
	assert.Equal(t, []Chunk{first, second, last}, claimed.InFlight, "in-flight chunks are reported in key order, not claim order")

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

// A chunk whose transaction failed is forgotten without landing: the
// frontier stays where the claim put it, so the applier keeps deferring
// changes for its keys, and the watermark cannot pass it.
func TestLedgerReleaseForgetsWithoutLanding(t *testing.T) {
	l := newLedger(Watermark{})
	first := mustChunk(t, math.MinInt64, 10)
	second := mustChunk(t, 11, 20)
	l.claim(first)
	l.claim(second)

	require.True(t, l.release(first))
	released := l.position()
	assert.Equal(t, []Chunk{second}, released.InFlight)
	assert.Equal(t, int64(20), released.Cut, "the frontier does not retreat on release")
	assert.False(t, released.Watermark.Valid())

	require.True(t, l.land(second, 10))
	assert.False(t, l.position().Watermark.Valid(), "the watermark waits for the released chunk to be copied again")

	assert.False(t, l.release(first), "a chunk is released at most once")
	assert.False(t, l.land(first, 1), "an unclaimed chunk cannot land")
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
	l.claim(next)
	require.True(t, l.land(next, 100))
	assert.Equal(t, NewWatermark(200), l.position().Watermark)

	complete := newLedger(NewWatermark(math.MaxInt64))
	assert.True(t, complete.complete, "a watermark at the largest key leaves nothing to copy")
	assert.Equal(t, NewWatermark(math.MaxInt64), complete.position().Watermark)
}
