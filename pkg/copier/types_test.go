package copier

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewChunk(t *testing.T) {
	c, err := NewChunk(-2, 4)
	require.NoError(t, err)
	assert.Equal(t, int64(-2), c.Lower())
	assert.Equal(t, int64(4), c.Upper())
	assert.Equal(t, int64(0), c.Rows(), "a range built by hand carries no cut size")
	_, err = NewChunk(4, 3)
	assert.EqualError(t, err, "chunk lower bound 4 exceeds upper bound 3")
}

func TestWatermarkStates(t *testing.T) {
	assert.False(t, Watermark{}.Valid(), "the zero watermark means nothing has been copied")
	w := NewWatermark(0)
	assert.True(t, w.Valid(), "a copied-through key of zero is still a valid watermark")
	assert.Equal(t, int64(0), w.Value())
	assert.Equal(t, int64(41), NewWatermark(41).Value())
}

// A watermark is complete only at the top of the key space, where the
// chunker's final open-above chunk ends; the zero watermark and every
// finite frontier below it leave keys on the other side.
func TestWatermarkComplete(t *testing.T) {
	assert.True(t, NewWatermark(math.MaxInt64).Complete())
	assert.False(t, NewWatermark(math.MaxInt64-1).Complete(), "one key remains")
	assert.False(t, NewWatermark(0).Complete())
	assert.False(t, Watermark{}.Complete(), "nothing copied is not everything copied")
}
