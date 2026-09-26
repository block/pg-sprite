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
	assert.EqualError(t, err, "invariant violation (ST-6): copy-and-swap target proof is empty")
}

func TestChunkerOptionsDefaults(t *testing.T) {
	opts := ChunkerOptions{}.withDefaults()
	assert.Equal(t, DefaultTargetChunkTime, opts.TargetChunkTime)
	assert.Equal(t, DefaultInitialChunkRows, opts.InitialRows)
	assert.Equal(t, DefaultMinChunkRows, opts.MinRows)
	assert.Equal(t, DefaultMaxChunkRows, opts.MaxRows)
	require.NoError(t, opts.validate())

	// One bound set on its own pulls the other defaults inside it, so the
	// filled options always validate.
	cases := map[string]struct {
		given ChunkerOptions
		want  ChunkerOptions
	}{
		"ceiling below the default floor": {
			ChunkerOptions{MaxRows: 7},
			ChunkerOptions{TargetChunkTime: DefaultTargetChunkTime, InitialRows: 7, MinRows: 7, MaxRows: 7},
		},
		"ceiling below the default initial size": {
			ChunkerOptions{MaxRows: 500},
			ChunkerOptions{TargetChunkTime: DefaultTargetChunkTime, InitialRows: 500, MinRows: DefaultMinChunkRows, MaxRows: 500},
		},
		"floor above the default ceiling": {
			ChunkerOptions{MinRows: 200_000},
			ChunkerOptions{TargetChunkTime: DefaultTargetChunkTime, InitialRows: 200_000, MinRows: 200_000, MaxRows: 200_000},
		},
		"floor above the default initial size": {
			ChunkerOptions{MinRows: 5000},
			ChunkerOptions{TargetChunkTime: DefaultTargetChunkTime, InitialRows: 5000, MinRows: 5000, MaxRows: DefaultMaxChunkRows},
		},
		"initial size alone is kept as given": {
			ChunkerOptions{InitialRows: 300},
			ChunkerOptions{TargetChunkTime: DefaultTargetChunkTime, InitialRows: 300, MinRows: DefaultMinChunkRows, MaxRows: DefaultMaxChunkRows},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.given.withDefaults()
			assert.Equal(t, tc.want, got)
			require.NoError(t, got.validate())
		})
	}

	// An explicitly set value is never moved, even when it cannot validate:
	// a floor the caller set above the ceiling the caller set is refused,
	// not repaired.
	contradictory := ChunkerOptions{MinRows: 50, MaxRows: 20}.withDefaults()
	assert.Equal(t, int64(50), contradictory.MinRows)
	assert.Equal(t, int64(20), contradictory.MaxRows)
	require.ErrorIs(t, contradictory.validate(), ErrInvalidChunkerOptions)

	// A negative bound is refused as given rather than fitted around.
	negative := ChunkerOptions{MaxRows: -5}.withDefaults()
	assert.Equal(t, DefaultMinChunkRows, negative.MinRows)
	assert.EqualError(t, negative.validate(), "invalid chunker options: maximum rows -5 must be positive")
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

// TestChunkerCutBeforeAnyQuery covers the frontier states that need no
// table: nothing cut, resumed past a watermark, and already complete. The
// state after each Next is asserted by the integration tests.
func TestChunkerCutBeforeAnyQuery(t *testing.T) {
	fresh := &Chunker{next: math.MinInt64}
	_, ok := fresh.Cut()
	assert.False(t, ok, "nothing has been cut before the first chunk")

	resumed := &Chunker{}
	resumed.next, resumed.done = startAfter(NewWatermark(3))
	upper, ok := resumed.Cut()
	assert.True(t, ok)
	assert.Equal(t, int64(3), upper, "a resumed chunker has cut through its watermark")

	// A watermark at the smallest key resumes from the key after it, and
	// that is a real cut, not the nothing-cut state.
	lowest := &Chunker{}
	lowest.next, lowest.done = startAfter(NewWatermark(math.MinInt64))
	upper, ok = lowest.Cut()
	assert.True(t, ok)
	assert.Equal(t, int64(math.MinInt64), upper)

	complete := &Chunker{}
	complete.next, complete.done = startAfter(NewWatermark(math.MaxInt64))
	upper, ok = complete.Cut()
	assert.True(t, ok)
	assert.Equal(t, int64(math.MaxInt64), upper, "a complete chunker has cut the whole key space")
}

// TestChunkerFeedbackSizesFromTheTimedChunk shows why Feedback scales the
// chunk it measured rather than the current size: several workers reporting
// the same measurement must agree on one next size, not multiply it.
func TestChunkerFeedbackSizesFromTheTimedChunk(t *testing.T) {
	opts := ChunkerOptions{}.withDefaults()
	c := &Chunker{opts: opts, rows: opts.InitialRows}
	timed := Chunk{lower: 1, upper: 1000, rows: 1000}

	// Four workers each copied a 1000-row chunk in a fifth of the target
	// time. Each report proposes 2000 (the step cap from 1000); had each
	// scaled the current size the result would be 16000.
	for range 4 {
		require.NoError(t, c.Feedback(timed, DefaultTargetChunkTime/5))
	}
	assert.Equal(t, int64(2000), c.Rows())

	// Two slow reports on the same chunk likewise agree on 500, not 250.
	for range 2 {
		require.NoError(t, c.Feedback(timed, 3*DefaultTargetChunkTime))
	}
	assert.Equal(t, int64(500), c.Rows())

	// A chunk carrying no cut size was not produced by a chunker and cannot
	// be sized from; the size is left alone.
	foreign, err := NewChunk(1, 1000)
	require.NoError(t, err)
	err = c.Feedback(foreign, DefaultTargetChunkTime)
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.EqualError(t, err, "invariant violation (ST-6): feedback for chunk [1, 1000] that no chunker cut")
	assert.Equal(t, int64(500), c.Rows())
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
