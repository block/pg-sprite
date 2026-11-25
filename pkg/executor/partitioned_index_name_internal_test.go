package executor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestDerivedPartitionIndexNameKeepsANameThatFits(t *testing.T) {
	assert.Equal(t, "idx_orders_region_p16401", derivedPartitionIndexName("idx_orders_region", 16401))
	exact := strings.Repeat("a", maxIdentifierBytes-len("_p7"))
	assert.Equal(t, exact+"_p7", derivedPartitionIndexName(exact, 7))
}

// A parent name that leaves no room is cut so the whole name is exactly the
// identifier limit, with a digest of the whole parent name before the OID
// suffix, which survives whole.
func TestDerivedPartitionIndexNameFitsTheIdentifierLimit(t *testing.T) {
	got := derivedPartitionIndexName(strings.Repeat("a", maxIdentifierBytes), 4294967295)

	assert.Len(t, got, maxIdentifierBytes)
	assert.Regexp(t, `^a+_[0-9a-f]{8}_p4294967295$`, got)
}

// Parents that differ only after the cut, and partitions of one parent, never
// share a name.
func TestDerivedPartitionIndexNameIsDistinct(t *testing.T) {
	prefix := strings.Repeat("c", 58)

	assert.NotEqual(t, derivedPartitionIndexName(prefix+"_x", 16401), derivedPartitionIndexName(prefix+"_y", 16401))
	assert.NotEqual(t, derivedPartitionIndexName(prefix+"_x", 16401), derivedPartitionIndexName(prefix+"_x", 16402))
}

// The cut never splits a multi-byte character, wherever the limit falls
// inside one.
func TestDerivedPartitionIndexNameCutsAtACharacterBoundary(t *testing.T) {
	for _, char := range []string{"é", "日", "😀"} {
		for pad := range 4 {
			got := derivedPartitionIndexName(strings.Repeat("a", pad)+strings.Repeat(char, maxIdentifierBytes), 16402)

			assert.True(t, utf8.ValidString(got), "pad %d, %q: %q", pad, char, got)
			assert.LessOrEqual(t, len(got), maxIdentifierBytes, "pad %d, %q", pad, char)
			assert.Greater(t, len(got), maxIdentifierBytes-len(char)-len("_p16402"), "pad %d, %q: cut more than one character", pad, char)
		}
	}
}
