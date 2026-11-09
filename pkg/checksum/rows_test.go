package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A row set derives the chunk digest the digest statement computes: the
// row count and the hex SHA-256 of the row hashes concatenated in key
// order, the SHA-256 of nothing for a range that holds no rows. The
// expected values are computed here from the hashes directly, not through
// the set.
func TestRowSetDigestMatchesTheDigestStatementsShape(t *testing.T) {
	empty := sha256.Sum256(nil)
	assert.Equal(t, Digest{Rows: 0, Hash: hex.EncodeToString(empty[:])}, rowSet{}.digest())

	a, b := hashOf('a'), hashOf('b')
	both := sha256.Sum256(append(append([]byte{}, a[:]...), b[:]...))
	assert.Equal(t, Digest{Rows: 2, Hash: hex.EncodeToString(both[:])}, rowSet{{1, a}, {2, b}}.digest())
	assert.NotEqual(t, rowSet{{1, a}, {2, b}}.digest(), rowSet{{1, b}, {2, a}}.digest(), "the digest is order-sensitive")
}

// A key the side holds reads as its hash and a key it does not hold reads
// as the zero hash, on either side of the set's keys and between them.
func TestRowSetAtReadsAbsentKeysAsZero(t *testing.T) {
	a, b := hashOf('a'), hashOf('b')
	s := rowSet{{10, a}, {30, b}}
	assert.Equal(t, a, s.at(10))
	assert.Equal(t, b, s.at(30))
	assert.Equal(t, rowHash{}, s.at(5))
	assert.Equal(t, rowHash{}, s.at(20))
	assert.Equal(t, rowHash{}, s.at(40))
	assert.Equal(t, rowHash{}, rowSet(nil).at(10))
}

// The keys to judge are every key any read holds on any side, once each,
// in key order, whatever order the sets were given in.
func TestUnionKeysDeduplicatesAcrossSets(t *testing.T) {
	a := hashOf('a')
	keys := unionKeys(rowSet{{3, a}, {5, a}}, rowSet{}, rowSet{{1, a}, {3, a}}, rowSet{{5, a}, {7, a}})
	assert.Equal(t, []int64{1, 3, 5, 7}, keys)
	assert.Nil(t, unionKeys(rowSet{}, nil))
}

// The server's chunk digest and the digest derived from the rows it
// returned in the same snapshot must agree; a disagreement is an invariant
// violation, since the pass would otherwise judge keys the digest did not
// describe.
func TestConfirmDigestFailsClosedOnDisagreement(t *testing.T) {
	rows := rowSet{{1, hashOf('a')}, {2, hashOf('b')}}
	require.NoError(t, confirmDigest("source", rows.digest(), rows))

	err := confirmDigest("source", Digest{Rows: 2, Hash: "other"}, rows)
	require.ErrorIs(t, err, ErrInvariantViolation)

	short := confirmDigest("shadow", rows[:1].digest(), rows)
	require.ErrorIs(t, short, ErrInvariantViolation)
}
