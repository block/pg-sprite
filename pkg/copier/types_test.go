package copier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewChunk(t *testing.T) {
	c, err := NewChunk(-2, 4)
	require.NoError(t, err)
	assert.Equal(t, int64(-2), c.Lower())
	assert.Equal(t, int64(4), c.Upper())
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
