package schemachange

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/schemadiff"
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

// Every name a deriver produces has the shape the managed-table listing
// hides, so a stopped run's shadow, a --keep-old retained source, and the
// retained source's renamed dependents are never offered to a directory
// owner as undeclared tables. The sources span schemas, a long name, and a
// quoted mixed-case name, since the hash must stay lowercase hex for all.
func TestEveryDerivedNameIsAnEngineRelationName(t *testing.T) {
	assert.Equal(t, "_pgsprite_c74da4a21f58f903_old", OldName("public", "orders"),
		"the retained source of public.orders, as the docs and the predicate's tests spell it")
	for _, source := range [][2]string{
		{"public", "orders"},
		{"sales", "Order Lines"},
		{"public", strings.Repeat("t", 63)},
	} {
		schema, table := source[0], source[1]
		assert.True(t, schemadiff.IsEngineRelationName(ShadowName(schema, table)), "shadow of %s.%s", schema, table)
		assert.True(t, schemadiff.IsEngineRelationName(OldName(schema, table)), "retained %s.%s", schema, table)
		assert.True(t, schemadiff.IsEngineRelationName(OldDependentName(schema, table, table+"_pkey")), "retained dependent of %s.%s", schema, table)
	}
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
