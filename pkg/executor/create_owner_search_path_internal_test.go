// White-box tests that the create-owner verification reads the real catalog
// on a session whose search_path lists a decoy schema ahead of pg_catalog.

package executor

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// createOwnerShadowFixture is a table t created by the session role in a
// throwaway schema, with the absence proof minted before it existed — the
// inputs verifyCreateOwner reads the committed owner from.
type createOwnerShadowFixture struct {
	url    string
	admin  *pgxpool.Pool
	schema string
	at     preflight.AbsentTarget
	owner  string
}

func newCreateOwnerShadowFixture(t *testing.T) createOwnerShadowFixture {
	t.Helper()
	url := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)
	at, err := preflight.CheckTableAbsent(t.Context(), admin, schema, "t")
	require.NoError(t, err)
	_, err = admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			a int
		)`, schema))
	require.NoError(t, err)
	var owner string
	require.NoError(t, admin.QueryRow(t.Context(), "SELECT current_user").Scan(&owner))
	return createOwnerShadowFixture{url: url, admin: admin, schema: schema, at: at, owner: owner}
}

// Decoy = operators on name and oid planted ahead of pg_catalog answer false
// for every pair. Resolved through search_path, the namespace and role joins
// and the name filters would match nothing and the verification would report
// the owner unverifiable; with every operator pg_catalog-qualified it reads
// the real catalog and confirms the session role owns the table.
func TestVerifyCreateOwnerComparesWithTheCatalogOperators(t *testing.T) {
	f := newCreateOwnerShadowFixture(t)
	_, err := f.admin.Exec(t.Context(), fmt.Sprintf(`
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
		)`, f.schema))
	require.NoError(t, err)
	shadowed := testutil.NewCatalogShadowingPool(t, f.url, f.schema)

	var nameEqual, oidEqual bool
	require.NoError(t, shadowed.QueryRow(t.Context(),
		`SELECT 'a'::name = 'a'::name, 1::oid = 1::oid`).Scan(&nameEqual, &oidEqual))
	require.False(t, nameEqual, "the decoy name operator must answer a bare = on this pool")
	require.False(t, oidEqual, "the decoy oid operator must answer a bare = on this pool")

	assert.NoError(t, verifyCreateOwner(t.Context(), shadowed, f.at, f.owner))
}

// Decoy pg_class and pg_namespace views planted ahead of pg_catalog answer
// differently from the catalog: the pg_class view names another role as the
// owner of every relation, and the pg_namespace view renames every schema.
// Read through either, the verification would report a mismatch or an
// unverifiable owner for a table the session role does own; qualified, it
// reads the real catalog and confirms the owner.
func TestVerifyCreateOwnerReadsTheCatalogRelations(t *testing.T) {
	f := newCreateOwnerShadowFixture(t)
	other := testutil.NewRole(t, f.admin, "NOLOGIN")
	var otherOID uint32
	require.NoError(t, f.admin.QueryRow(t.Context(),
		`SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1`, other).Scan(&otherOID))
	_, err := f.admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE VIEW %[1]s.pg_class AS
		SELECT relname,
		       relnamespace,
		       %[2]d::oid AS relowner
		FROM pg_catalog.pg_class;
		CREATE VIEW %[1]s.pg_namespace AS
		SELECT oid,
		       'elsewhere'::name AS nspname
		FROM pg_catalog.pg_namespace`, f.schema, otherOID))
	require.NoError(t, err)
	shadowed := testutil.NewCatalogShadowingPool(t, f.url, f.schema)

	var decoyOwner uint32
	require.NoError(t, shadowed.QueryRow(t.Context(),
		`SELECT relowner FROM pg_class WHERE relname = 't'`).Scan(&decoyOwner))
	require.Equal(t, otherOID, decoyOwner, "the decoy pg_class must answer an unqualified read on this pool")
	var decoyNamespace string
	require.NoError(t, shadowed.QueryRow(t.Context(),
		`SELECT nspname FROM pg_namespace LIMIT 1`).Scan(&decoyNamespace))
	require.Equal(t, "elsewhere", decoyNamespace, "the decoy pg_namespace must answer an unqualified read on this pool")

	assert.NoError(t, verifyCreateOwner(t.Context(), shadowed, f.at, f.owner))
}
