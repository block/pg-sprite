package copier

import (
	"fmt"
	"math"
)

// Chunk is a closed range of a single-column integer primary key. Its fields
// are unexported so that lower <= upper holds for every value the chunker
// can observe; NewChunk is the only way to obtain a non-zero Chunk.
type Chunk struct {
	lower int64
	upper int64
	// rows is the row count a Chunker cut this chunk to; zero for a chunk
	// built by NewChunk, which sizes nothing.
	rows int64
}

// NewChunk validates and returns the closed range [lower, upper].
func NewChunk(lower, upper int64) (Chunk, error) {
	if lower > upper {
		return Chunk{}, fmt.Errorf("chunk lower bound %d exceeds upper bound %d", lower, upper)
	}
	return Chunk{lower: lower, upper: upper}, nil
}

// Lower returns the inclusive lower bound.
func (c Chunk) Lower() int64 { return c.lower }

// Upper returns the inclusive upper bound.
func (c Chunk) Upper() int64 { return c.upper }

// Rows returns the row count a Chunker cut the chunk to: the number of keys
// it holds, except for the final open-above chunk, which holds fewer. It is
// zero for a chunk not cut by a Chunker.
func (c Chunk) Rows() int64 { return c.rows }

// Watermark is a frontier in the primary-key space: every key at or below
// its value is on one side of the copy. The copier reports two — the
// copied-through watermark (every key at or below it has landed) and the cut
// frontier (every key at or below it is in a chunk a worker has started
// reading). The zero value means the frontier has not been reached, so
// there is no key on that side; a valid watermark is obtainable only from
// NewWatermark, so a value can never be paired with the wrong validity flag.
type Watermark struct {
	value int64
	valid bool
}

// NewWatermark returns a valid watermark at value.
func NewWatermark(value int64) Watermark { return Watermark{value: value, valid: true} }

// Valid reports whether the frontier has been reached. Value is meaningful
// only when Valid is true.
func (w Watermark) Valid() bool { return w.valid }

// Value returns the frontier's primary key. It is zero for an invalid
// watermark; callers check Valid first.
func (w Watermark) Value() int64 { return w.value }

// Complete reports whether the frontier is past every key an int64 can
// hold: the chunker's final open-above chunk has landed, so no key remains
// on the other side of the watermark.
func (w Watermark) Complete() bool { return w.valid && w.value == math.MaxInt64 }
