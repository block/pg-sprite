package schemachange_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The fence's source check runs under the caller's session search_path, so
// it resolves every catalog name through pg_catalog (CO-9). The pool here is
// a library caller's own, with the table's schema ahead of pg_catalog and an
// impostor pg_get_userbyid in that schema that names the proof's owner for
// every relation. The source changed hands after the proof was minted, so
// the check must refuse it as a foreign relation (ST-5) from the real owner;
// an impostor that answered would let the fence be taken off a table the
// proof was not minted for.
func TestDropWriteFenceIgnoresTheSessionSearchPath(t *testing.T) {
	f, newOwner := newShadowFixtureWithRole(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	f.exec(t, "ALTER TABLE %s.orders OWNER TO "+pgx.Identifier{newOwner}.Sanitize())
	f.exec(t, `
		CREATE FUNCTION %s.pg_get_userbyid(oid) RETURNS name
		LANGUAGE sql STABLE
		AS 'SELECT ''`+strings.ReplaceAll(target.OwnerRole(), "'", "''")+`''::name'`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)

	var seen string
	require.NoError(t, shadowing.QueryRow(t.Context(), `
		SELECT pg_get_userbyid(c.relowner)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = 'orders'`, f.schema).Scan(&seen))
	require.Equal(t, target.OwnerRole(), seen, "the impostor answers an unqualified read on this pool")

	err := schemachange.DropWriteFence(t.Context(), shadowing, lock, target, schemachange.Options{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseForeignRelation, schemachange.RefusalCauseOf(err))
}
