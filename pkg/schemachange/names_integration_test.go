package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
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

// The lookup runs under the caller's session search_path, so it resolves the
// relation list through pg_catalog (CO-9). The pool here is a library
// caller's own, with the schema ahead of pg_catalog and an impostor "char"
// equality in that schema that is never true: an unqualified relkind filter
// would list no table, and a shadow the engine built would read as a name no
// table derives.
func TestSourceOfDerivedNameIgnoresTheSessionSearchPath(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE FUNCTION %s.never_equal("char", "char") RETURNS boolean
		LANGUAGE sql IMMUTABLE
		AS 'SELECT false'`)
	f.exec(t, `CREATE OPERATOR %s.= (LEFTARG = "char", RIGHTARG = "char", FUNCTION = %s.never_equal)`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)

	var equal bool
	require.NoError(t, shadowing.QueryRow(t.Context(), `SELECT 'r'::"char" = 'r'::"char"`).Scan(&equal))
	require.False(t, equal, "the impostor answers an unqualified comparison on this pool")

	table, kind, err := schemachange.SourceOfDerivedName(t.Context(), shadowing, f.schema, schemachange.ShadowName(f.schema, "widgets"))
	require.NoError(t, err)
	assert.Equal(t, "widgets", table)
	assert.Equal(t, schemachange.DerivedShadow, kind)
}
