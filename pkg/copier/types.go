package copier

import "fmt"

// Chunk is a closed range of a single-column integer primary key.
type Chunk struct {
	Lower int64
	Upper int64
}

// NewChunk validates and returns the closed range [lower, upper].
func NewChunk(lower, upper int64) (Chunk, error) {
	if lower > upper {
		return Chunk{}, fmt.Errorf("chunk lower bound %d exceeds upper bound %d", lower, upper)
	}
	return Chunk{Lower: lower, Upper: upper}, nil
}

// Watermark identifies the highest primary key below which every chunk was copied.
// Valid is false when no chunk has yet been copied; Value is then ignored.
type Watermark struct {
	Value int64
	Valid bool
}

// NewWatermark returns a valid copied-through watermark.
func NewWatermark(value int64) Watermark { return Watermark{Value: value, Valid: true} }
