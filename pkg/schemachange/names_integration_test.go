package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// A derived name has no inverse, but its source is always a table in the
// same schema: the lookup recomputes each table's derived names and returns
// the one that matches, distinguishing a shadow from a retained table. A
// name no table derives, and a matching name in a different schema, are
// both ErrNotDerivedName.
func TestSourceOfDerivedNameFindsTheTableInTheSameSchema(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY
		)`)

	table, kind, err := schemachange.SourceOfDerivedName(t.Context(), f.pool, f.schema, schemachange.ShadowName(f.schema, "widgets"))
	require.NoError(t, err)
	assert.Equal(t, "widgets", table)
	assert.Equal(t, schemachange.DerivedShadow, kind)

	table, kind, err = schemachange.SourceOfDerivedName(t.Context(), f.pool, f.schema, schemachange.OldName(f.schema, "orders"))
	require.NoError(t, err)
	assert.Equal(t, "orders", table)
	assert.Equal(t, schemachange.DerivedOld, kind)

	_, _, err = schemachange.SourceOfDerivedName(t.Context(), f.pool, f.schema, "_pgsprite_0000000000000000_new")
	require.ErrorIs(t, err, schemachange.ErrNotDerivedName)

	_, _, err = schemachange.SourceOfDerivedName(t.Context(), f.pool, f.schema, schemachange.ShadowName("public", "widgets"))
	require.ErrorIs(t, err, schemachange.ErrNotDerivedName, "the same table name in another schema derives a different name")
}
