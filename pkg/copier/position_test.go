package copier

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Classify is the CO-4 discard rule: only the cut frontier decides what is
// uncut, in-flight chunks decide what must wait, and everything else at or
// below the frontier has landed — including landed chunks above the
// watermark and the resumed prefix below it.
func TestPositionClassifiesKeysAgainstTheCutFrontier(t *testing.T) {
	pos := Position{
		Watermark: NewWatermark(10),
		Cut:       NewWatermark(30),
		InFlight:  []Chunk{mustChunk(t, 11, 20)},
	}
	cases := map[int64]KeyState{
		math.MinInt64: KeyLanded,
		10:            KeyLanded,
		11:            KeyInFlight,
		20:            KeyInFlight,
		21:            KeyLanded,
		30:            KeyLanded,
		31:            KeyUncut,
		math.MaxInt64: KeyUncut,
	}
	for key, want := range cases {
		assert.Equal(t, want, pos.Classify(key), "key %d", key)
	}

	nothingCut := Position{}
	assert.Equal(t, KeyUncut, nothingCut.Classify(math.MinInt64), "before the first claim every key is uncut")
	assert.Equal(t, KeyUncut, nothingCut.Classify(0))
}

func TestKeyStateStrings(t *testing.T) {
	assert.Equal(t, "uncut", KeyUncut.String())
	assert.Equal(t, "in-flight", KeyInFlight.String())
	assert.Equal(t, "landed", KeyLanded.String())
	assert.Equal(t, "unknown", KeyState(99).String())
}
