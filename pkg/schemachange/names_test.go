package schemachange

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hash keys on the qualified name: the same table in two schemas derives
// two different shadows, and the same qualified name always derives the same
// one, so a resumed run finds the shadow an earlier run built.
func TestNameHashKeysOnQualifiedName(t *testing.T) {
	assert.Equal(t, "eee6bd2f", NameHash("public", "widgets"))
	assert.NotEqual(t, NameHash("public", "widgets"), NameHash("sales", "widgets"))
	assert.Equal(t, NameHash("public", "widgets"), NameHash("public", "widgets"))
}

// Every derived name shares one prefix and one hash, and the dependents'
// retained names hang off the retained table's name.
func TestDerivedNamesShareOneHash(t *testing.T) {
	hash := NameHash("public", "widgets")
	assert.Equal(t, "_pgsprite_"+hash+"_new", ShadowName("public", "widgets"))
	assert.Equal(t, "_pgsprite_"+hash+"_old", OldName("public", "widgets"))
	assert.Equal(t, "_pgsprite_"+hash+"_old_widgets_pkey", OldDependentName("public", "widgets", "widgets_pkey"))
}

// The limit is measured in bytes, as the server measures it: a 63-byte name
// passes, a 64-byte one is refused, and a 32-rune name of two-byte runes is
// refused because it encodes to 64 bytes.
func TestCheckIdentifierLengthsMeasuresBytes(t *testing.T) {
	require.NoError(t, CheckIdentifierLengths(strings.Repeat("a", 63)))
	require.ErrorIs(t, CheckIdentifierLengths(strings.Repeat("a", 64)), ErrNameTooLong)
	require.ErrorIs(t, CheckIdentifierLengths("short", strings.Repeat("é", 32)), ErrNameTooLong)
}
