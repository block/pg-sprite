package schemachange

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The hash keys on the qualified name: the same table in two schemas derives
// two different shadows, and the same qualified name always derives the same
// one, so a resumed run finds the shadow an earlier run built.
func TestNameHashKeysOnQualifiedName(t *testing.T) {
	assert.Equal(t, "b79ff6d5d91cfdaf", NameHash("public", "widgets"))
	assert.NotEqual(t, NameHash("public", "widgets"), NameHash("sales", "widgets"))
	assert.Equal(t, NameHash("public", "widgets"), NameHash("public", "widgets"))
}

// Every derived name shares one prefix and one hash, and the dependents'
// retained names hang off the retained table's name with the dependent
// hashed the same way.
func TestDerivedNamesShareOneHash(t *testing.T) {
	hash := NameHash("public", "widgets")
	assert.Equal(t, "_pgsprite_"+hash+"_new", ShadowName("public", "widgets"))
	assert.Equal(t, "_pgsprite_"+hash+"_old", OldName("public", "widgets"))
	assert.Equal(t, "_pgsprite_"+hash+"_old_"+nameHash("widgets_pkey"), OldDependentName("public", "widgets", "widgets_pkey"))
	assert.NotEqual(t, OldDependentName("public", "widgets", "widgets_pkey"), OldDependentName("public", "widgets", "widgets_sku_idx"))
}

// Every derived name is a fixed 30 or 47 bytes regardless of how long the
// source names are, so no source name can push a derived name past
// PostgreSQL's 63-byte identifier limit into silent truncation. The
// dependent is measured at the longest name the server itself accepts.
func TestDerivedNamesHaveFixedWidthUnderTheIdentifierLimit(t *testing.T) {
	table := strings.Repeat("t", 63)
	dependent := strings.Repeat("d", 63)
	assert.Len(t, ShadowName("public", table), 30)
	assert.Len(t, OldName("public", table), 30)
	assert.Len(t, OldDependentName("public", table, dependent), 47)
	assert.Len(t, OldDependentName("public", "w", "i"), 47)
}
