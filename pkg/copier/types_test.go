package copier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewChunk(t *testing.T) {
	c, err := NewChunk(-2, 4)
	require.NoError(t, err)
	assert.Equal(t, Chunk{Lower: -2, Upper: 4}, c)
	_, err = NewChunk(4, 3)
	assert.EqualError(t, err, "chunk lower bound 4 exceeds upper bound 3")
}

func TestWatermarkStates(t *testing.T) {
	assert.Equal(t, Watermark{}, Watermark{})
	assert.Equal(t, Watermark{Value: 0, Valid: true}, NewWatermark(0))
}
