// White-box test that the row security privilege check reads the real
// catalog on a session whose search_path lists a decoy schema ahead of
// pg_catalog.

package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// Decoy = operators on name and oid planted ahead of pg_catalog answer false
// for every pair. Resolved through search_path, the namespace join and the
// name filters would match nothing and the check would refuse an existing
// table as not found; with every operator pg_catalog-qualified it reads the
// real catalog and admits the table its owner checks.
func TestCheckRowSecurityPrivilegesComparesWithTheCatalogOperators(t *testing.T) {
	url := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)
	_, err = admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (
			a int
		);
		CREATE FUNCTION %[1]s.never(name, name) RETURNS boolean
			LANGUAGE sql IMMUTABLE AS 'SELECT false';
		CREATE OPERATOR %[1]s.= (
			LEFTARG = name,
			RIGHTARG = name,
			FUNCTION = %[1]s.never
		);
		CREATE FUNCTION %[1]s.never(oid, oid) RETURNS boolean
			LANGUAGE sql IMMUTABLE AS 'SELECT false';
		CREATE OPERATOR %[1]s.= (
			LEFTARG = oid,
			RIGHTARG = oid,
			FUNCTION = %[1]s.never
		)`, schema))
	require.NoError(t, err)
	shadowed := testutil.NewCatalogShadowingPool(t, url, schema)

	var nameEqual, oidEqual bool
	require.NoError(t, shadowed.QueryRow(t.Context(),
		`SELECT 'a'::name = 'a'::name, 1::oid = 1::oid`).Scan(&nameEqual, &oidEqual))
	require.False(t, nameEqual, "the decoy name operator must answer a bare = on this pool")
	require.False(t, oidEqual, "the decoy oid operator must answer a bare = on this pool")

	tx, err := shadowed.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
	})

	assert.NoError(t, checkRowSecurityPrivileges(t.Context(), tx, schema, "t"))
}
