package testutil

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// NewLookalikeNameMatchPool plants, in a fresh schema of the database at
// databaseURL, an = on name and name and a LIKE (~~) on name and text
// that answer true for every pair, and returns a pool whose search_path
// lists that schema ahead of pg_catalog. On that pool an unqualified
// comparison of a slot's database or name matches every slot on the
// cluster, so a read that judges a slot by its database or name through
// it is proven to spell its operators OPERATOR(pg_catalog.…). The pool
// is asserted to reach both lookalikes before it is returned.
func NewLookalikeNameMatchPool(t *testing.T, databaseURL string, owner *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	schema := NewSchema(t, owner)
	for _, ddl := range []string{
		`
		CREATE FUNCTION %[1]s.always_name_eq(name, name) RETURNS boolean
		LANGUAGE sql AS 'SELECT true'`,
		`
		CREATE OPERATOR %[1]s.= (
			LEFTARG  = name,
			RIGHTARG = name,
			FUNCTION = %[1]s.always_name_eq
		)`,
		`
		CREATE FUNCTION %[1]s.always_name_like(name, text) RETURNS boolean
		LANGUAGE sql AS 'SELECT true'`,
		`
		CREATE OPERATOR %[1]s.~~ (
			LEFTARG  = name,
			RIGHTARG = text,
			FUNCTION = %[1]s.always_name_like
		)`,
	} {
		_, err := owner.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, "plant a lookalike name operator")
	}
	pool := NewCatalogShadowingPool(t, databaseURL, schema)

	var equal, like bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT 'one'::name = 'other'::name, 'one'::name LIKE 'other'`).Scan(&equal, &like))
	require.True(t, equal, "the lookalike name = name must answer on this pool")
	require.True(t, like, "the lookalike name LIKE text must answer on this pool")
	return pool
}
