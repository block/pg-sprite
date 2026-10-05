package schemachange

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A bound that equals the old type's own limit follows the new type; a
// bound anyone declared stays as written. Each bound is judged on its own,
// so an ascending integer sequence (min 1, max 2^31-1) widened to bigint
// keeps its 1 and takes the bigint maximum, the way ALTER SEQUENCE … AS
// bigint would.
func TestRebaseSequenceBoundsMovesOnlyTheTypeDerivedBounds(t *testing.T) {
	ascending := SequenceOptions{Start: 1, Increment: 1, Min: 1, Max: math.MaxInt32, Cache: 1}
	assert.Equal(t,
		SequenceOptions{Start: 1, Increment: 1, Min: 1, Max: math.MaxInt64, Cache: 1},
		rebaseSequenceBounds(ascending, int4TypeOID, int8TypeOID))

	descending := SequenceOptions{Start: -1, Increment: -1, Min: math.MinInt16, Max: -1, Cache: 1}
	assert.Equal(t,
		SequenceOptions{Start: -1, Increment: -1, Min: math.MinInt32, Max: -1, Cache: 1},
		rebaseSequenceBounds(descending, int2TypeOID, int4TypeOID))

	declared := SequenceOptions{Start: 1, Increment: 1, Min: 1, Max: 1_000_000, Cache: 1}
	assert.Equal(t, declared, rebaseSequenceBounds(declared, int4TypeOID, int8TypeOID),
		"a maximum the user declared is not a type limit and stays")

	assert.Equal(t, ascending, rebaseSequenceBounds(ascending, int4TypeOID, int4TypeOID),
		"an unchanged type changes nothing")
}

// Narrowing moves a type-derived bound down as well: the server does the
// same, and the handoff must declare what the live column's type admits.
func TestRebaseSequenceBoundsNarrowsAsWell(t *testing.T) {
	wide := SequenceOptions{Start: 1, Increment: 1, Min: 1, Max: math.MaxInt64, Cache: 1}
	assert.Equal(t,
		SequenceOptions{Start: 1, Increment: 1, Min: 1, Max: math.MaxInt32, Cache: 1},
		rebaseSequenceBounds(wide, int8TypeOID, int4TypeOID))
}

// A type that is not one of the integer types leaves the options alone:
// there is no limit to recognise.
func TestRebaseSequenceBoundsIgnoresNonIntegerTypes(t *testing.T) {
	opts := SequenceOptions{Start: 1, Increment: 1, Min: 1, Max: math.MaxInt32, Cache: 1}
	const numericTypeOID uint32 = 1700
	assert.Equal(t, opts, rebaseSequenceBounds(opts, int4TypeOID, numericTypeOID))
	assert.Equal(t, opts, rebaseSequenceBounds(opts, numericTypeOID, int8TypeOID))

	_, _, ok := integerTypeBounds(numericTypeOID)
	assert.False(t, ok)
}
