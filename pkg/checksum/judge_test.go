package checksum

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// hashOf is a distinct row hash per letter, for tests that only need to
// tell rows apart.
func hashOf(letter byte) rowHash {
	var h rowHash
	h[0] = letter
	return h
}

// Two reads of one key, the second taken after the catch-up confirmed past
// the first, are judged together: agreement or the shadow reaching the
// first read's source settles the key, movement on either side means
// another read, and stillness on both sides with a difference is the
// divergence. A side without the row reads as the zero hash, so a key
// inserted since the first read is settled like any change the first read
// saw before, a key deleted on the source and gone from the shadow is
// equal, and a key the shadow holds that the source never had is a
// divergence.
func TestJudgeVerdicts(t *testing.T) {
	a, b, c, d := hashOf('a'), hashOf('b'), hashOf('c'), hashOf('d')
	var absent rowHash
	read := func(source, shadow rowHash) keyPair {
		return keyPair{source: source, shadow: shadow}
	}
	cases := map[string]struct {
		prior, now keyPair
		want       verdict
	}{
		"shadow caught up to the moved source":     {read(a, b), read(c, c), verdictEqual},
		"shadow reached what the sweep saw":        {read(a, b), read(c, a), verdictSettled},
		"source moved and shadow still behind":     {read(a, b), read(c, b), verdictHot},
		"source still and shadow moved elsewhere":  {read(a, b), read(a, d), verdictHot},
		"both moved and still apart":               {read(a, b), read(c, d), verdictHot},
		"both still across the confirmed position": {read(a, b), read(a, b), verdictDiverged},
		"inserted on the source since the sweep":   {read(absent, absent), read(a, absent), verdictSettled},
		"insert reached the shadow":                {read(a, absent), read(a, a), verdictEqual},
		"insert changed again before it reached":   {read(a, absent), read(c, absent), verdictHot},
		"deleted on both sides":                    {read(a, a), read(absent, absent), verdictEqual},
		"deleted on the source, shadow behind":     {read(a, b), read(absent, b), verdictHot},
		"shadow holds a row the source never had":  {read(absent, b), read(absent, b), verdictDiverged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, judge(tc.prior, tc.now))
		})
	}
}

// A chunk is judged key by key over the keys it was asked about: the keys
// still hot come back in the order given, the divergences are counted, and
// a key inserted since the first read is settled and a key neither read
// holds on either side judges as equal absences. The
// settled keys of a chunk are not reported, since the caller has nothing
// left to do with them.
func TestJudgeKeysSeparatesHotFromDiverged(t *testing.T) {
	chunk, err := copier.NewChunk(1, 10)
	require.NoError(t, err)
	a, b, c := hashOf('a'), hashOf('b'), hashOf('c')
	prior := observation{chunk: chunk,
		source: rowSet{{1, a}, {2, a}, {3, a}, {5, a}},
		shadow: rowSet{{1, b}, {2, b}, {3, b}, {4, b}},
	}
	now := observation{chunk: chunk,
		source: rowSet{{1, c}, {2, a}, {3, c}, {5, c}, {6, a}},
		shadow: rowSet{{1, c}, {2, b}, {3, a}, {4, b}},
	}
	hot, diverged := judgeKeys(prior, now, []int64{1, 2, 3, 4, 5, 6, 7})
	assert.Equal(t, []int64{5}, hot, "key 5 changed on the source before its insert reached the shadow")
	assert.Equal(t, 2, diverged, "keys 2 and 4 held still on both sides and still differ")

	hot, diverged = judgeKeys(prior, now, []int64{1, 3})
	assert.Empty(t, hot, "key 1 is equal and key 3's shadow reached the earlier source")
	assert.Zero(t, diverged)
}
