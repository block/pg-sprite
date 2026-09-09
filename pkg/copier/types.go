package copier

import "fmt"

// Chunk is a closed range of a single-column integer primary key. Its fields
// are unexported so that lower <= upper holds for every value the chunker
// can observe; NewChunk is the only way to obtain a non-zero Chunk.
type Chunk struct {
	lower int64
	upper int64
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

// Watermark identifies the highest primary key below which every chunk was
// copied. The zero value means no chunk has been copied yet; a valid
// watermark is obtainable only from NewWatermark, so a value can never be
// paired with the wrong validity flag.
type Watermark struct {
	value int64
	valid bool
}

// NewWatermark returns a valid copied-through watermark.
func NewWatermark(value int64) Watermark { return Watermark{value: value, valid: true} }

// Valid reports whether any chunk has been copied. Value is meaningful only
// when Valid is true.
func (w Watermark) Valid() bool { return w.valid }

// Value returns the copied-through primary key. It is zero for an invalid
// watermark; callers check Valid first.
func (w Watermark) Value() int64 { return w.value }
