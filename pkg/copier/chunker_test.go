package copier

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/preflight"
)

func TestNewChunkerRejectsEmptyProof(t *testing.T) {
	_, err := NewChunker(preflight.CopySwapTarget{}, Watermark{}, ChunkerOptions{})
	require.ErrorIs(t, err, ErrInvariantViolation)
}

func TestChunkerOptionsDefaults(t *testing.T) {
	opts := ChunkerOptions{}.withDefaults()
	assert.Equal(t, DefaultTargetChunkTime, opts.TargetChunkTime)
	assert.Equal(t, DefaultInitialChunkRows, opts.InitialRows)
	assert.Equal(t, DefaultMinChunkRows, opts.MinRows)
	assert.Equal(t, DefaultMaxChunkRows, opts.MaxRows)
	require.NoError(t, opts.validate())

	// An explicit value is kept; only zero takes the default.
	partial := ChunkerOptions{MaxRows: 7}.withDefaults()
	assert.Equal(t, int64(7), partial.MaxRows)
	assert.Equal(t, DefaultMinChunkRows, partial.MinRows)
}

func TestChunkerOptionsValidate(t *testing.T) {
	valid := ChunkerOptions{TargetChunkTime: time.Second, InitialRows: 10, MinRows: 5, MaxRows: 20}
	require.NoError(t, valid.validate())

	cases := map[string]struct {
		mutate func(*ChunkerOptions)
		detail string
	}{
		"negative target time":  {func(o *ChunkerOptions) { o.TargetChunkTime = -time.Second }, "target chunk time -1s must be positive"},
		"zero initial rows":     {func(o *ChunkerOptions) { o.InitialRows = 0 }, "initial rows 0 must be positive"},
		"negative min rows":     {func(o *ChunkerOptions) { o.MinRows = -1 }, "minimum rows -1 must be positive"},
		"zero max rows":         {func(o *ChunkerOptions) { o.MaxRows = 0 }, "maximum rows 0 must be positive"},
		"floor above ceiling":   {func(o *ChunkerOptions) { o.MinRows = 21 }, "minimum rows 21 exceeds maximum rows 20"},
		"initial below floor":   {func(o *ChunkerOptions) { o.InitialRows = 4 }, "initial rows 4 is outside [5, 20]"},
		"initial above ceiling": {func(o *ChunkerOptions) { o.InitialRows = 21 }, "initial rows 21 is outside [5, 20]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := valid
			tc.mutate(&opts)
			err := opts.validate()
			require.ErrorIs(t, err, ErrInvalidChunkerOptions)
			assert.EqualError(t, err, "invalid chunker options: "+tc.detail)
		})
	}
}

func TestStartAfter(t *testing.T) {
	lower, done := startAfter(Watermark{})
	assert.Equal(t, int64(math.MinInt64), lower, "nothing copied covers the key space from its smallest value")
	assert.False(t, done)

	lower, done = startAfter(NewWatermark(41))
	assert.Equal(t, int64(42), lower)
	assert.False(t, done)

	// A copied-through watermark of zero is a real position, not the zero
	// watermark: the next chunk starts at one.
	lower, done = startAfter(NewWatermark(0))
	assert.Equal(t, int64(1), lower)
	assert.False(t, done)

	// The last chunk is open above, so a watermark at the largest key means
	// the copy is complete; there is no key after it to overflow into.
	_, done = startAfter(NewWatermark(math.MaxInt64))
	assert.True(t, done)
}

func TestNextRows(t *testing.T) {
	opts := ChunkerOptions{TargetChunkTime: time.Second, InitialRows: 1000, MinRows: 100, MaxRows: 5000}

	// Half the target time doubles; double the target time halves.
	assert.Equal(t, int64(2000), nextRows(1000, 500*time.Millisecond, opts))
	assert.Equal(t, int64(500), nextRows(1000, 2*time.Second, opts))
	// Exactly on target leaves the size alone.
	assert.Equal(t, int64(1000), nextRows(1000, time.Second, opts))
	// A fractional ratio scales proportionally and rounds.
	assert.Equal(t, int64(1250), nextRows(1000, 800*time.Millisecond, opts))

	// One step never moves by more than a factor of two, however far off
	// the chunk was.
	assert.Equal(t, int64(2000), nextRows(1000, time.Millisecond, opts), "a very fast chunk grows by the maximum step only")
	assert.Equal(t, int64(500), nextRows(1000, time.Minute, opts), "a very slow chunk shrinks by the maximum step only")

	// The configured bounds win over the step clamp.
	assert.Equal(t, int64(5000), nextRows(4000, 500*time.Millisecond, opts))
	assert.Equal(t, int64(100), nextRows(150, 2*time.Second, opts))

	// A non-positive elapsed is treated as instantaneous, not as a division
	// by zero or a shrink.
	assert.Equal(t, int64(2000), nextRows(1000, 0, opts))
	assert.Equal(t, int64(2000), nextRows(1000, -time.Second, opts))
}
