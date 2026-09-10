package schemadiff

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// sqlstateInsufficientPrivilege is the SQLSTATE the server raises when the
// session's role may not perform the statement.
const sqlstateInsufficientPrivilege = "42501"

func TestListManagedTables(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("lists file-backed table kinds", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		qualified := pgx.Identifier{schema}.Sanitize()
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.ordinary (id bigint);
			CREATE TABLE %[1]s.partitioned (id bigint) PARTITION BY RANGE (id);
			CREATE TABLE %[1]s.partitioned_first PARTITION OF %[1]s.partitioned
				FOR VALUES FROM (0) TO (10);
			CREATE TABLE %[1]s.inheritance_parent (id bigint);
			CREATE TABLE %[1]s.inheritance_child () INHERITS (%[1]s.inheritance_parent);
			CREATE UNLOGGED TABLE %[1]s.unlogged (id bigint);
			CREATE VIEW %[1]s.a_view AS SELECT 1 AS one;
			CREATE MATERIALIZED VIEW %[1]s.a_matview AS SELECT 1 AS one;
			CREATE SEQUENCE %[1]s.a_sequence;`, qualified))
		require.NoError(t, err)

		tables, err := ListManagedTables(t.Context(), pool, schema)
		require.NoError(t, err)
		assert.Equal(t, []string{
			"inheritance_child",
			"inheritance_parent",
			"ordinary",
			"partitioned",
			"unlogged",
		}, tables)
	})

	t.Run("empty schema", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)

		tables, err := ListManagedTables(t.Context(), pool, schema)
		require.NoError(t, err)
		assert.Empty(t, tables)
		assert.NotNil(t, tables)
	})
}

// An extension's member tables are created and dropped with the extension,
// so no desired file declares them. The stock integration image ships no
// extension that creates a table, so the test makes one a member of the
// extension every database carries — the catalog records the same
// extension dependency an extension script would — and proves the listing
// leaves it out while an unowned neighbour stays in.
func TestListManagedTablesExcludesExtensionOwnedRelations(t *testing.T) {
	pool := newDatabasePool(t)
	_, err := pool.Exec(t.Context(), `
		CREATE SCHEMA app;
		CREATE TABLE app.owned_by_ext (id bigint);
		CREATE TABLE app.unowned (id bigint)`)
	require.NoError(t, err)
	addToExtensionOrSkip(t, pool, "app.owned_by_ext")

	tables, err := ListManagedTables(t.Context(), pool, "app")
	require.NoError(t, err)

	assert.Equal(t, []string{"unowned"}, tables)
}

// A user schema listed ahead of pg_catalog on search_path shadows every
// unqualified catalog name the query could use. These impostors answer
// wrongly without erroring: the relations are empty and the operators are
// always false. Consulted on the way to the table list they list no
// tables; consulted inside the extension filter they match no dependency
// and let the extension member through as a false positive; the regclass
// impostor cannot parse the catalog's name, so an unqualified cast cannot
// silently match the dependency either. The enumeration must see the real
// catalog through all of them.
func TestListManagedTablesResistsCatalogShadowing(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	bootstrap, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(bootstrap.Close)
	_, err = bootstrap.Exec(t.Context(), `
		CREATE SCHEMA shadow;
		CREATE TABLE shadow.pg_class (oid oid, relname name, relnamespace oid, relkind "char", relispartition boolean);
		CREATE TABLE shadow.pg_namespace (oid oid, nspname name);
		CREATE TABLE shadow.pg_depend (classid oid, objid oid, deptype "char");
		CREATE DOMAIN shadow.regclass AS oid;
		CREATE FUNCTION shadow.never_oid(oid, oid) RETURNS boolean
			LANGUAGE sql IMMUTABLE AS 'SELECT false';
		CREATE FUNCTION shadow.never_name(name, name) RETURNS boolean
			LANGUAGE sql IMMUTABLE AS 'SELECT false';
		CREATE FUNCTION shadow.never_char("char", "char") RETURNS boolean
			LANGUAGE sql IMMUTABLE AS 'SELECT false';
		CREATE OPERATOR shadow.= (LEFTARG = oid, RIGHTARG = oid, FUNCTION = shadow.never_oid);
		CREATE OPERATOR shadow.= (LEFTARG = name, RIGHTARG = name, FUNCTION = shadow.never_name);
		CREATE OPERATOR shadow.= (LEFTARG = "char", RIGHTARG = "char", FUNCTION = shadow.never_char);
		CREATE SCHEMA app;
		CREATE TABLE app.ordinary (id bigint);
		CREATE TABLE app.partitioned (id bigint) PARTITION BY RANGE (id);
		CREATE TABLE app.partitioned_first PARTITION OF app.partitioned
			FOR VALUES FROM (0) TO (10);
		CREATE TABLE app.owned_by_ext (id bigint)`)
	require.NoError(t, err)
	addToExtensionOrSkip(t, bootstrap, "app.owned_by_ext")

	// A raw pool: dbconn.NewPool removes a shadowed pg_catalog from the
	// search_path on connect, which would disarm this test.
	pool := testutil.NewCatalogShadowingPool(t, url, "shadow")

	tables, err := ListManagedTables(t.Context(), pool, "app")
	require.NoError(t, err, "the enumeration must see the real catalog through the impostors")

	assert.Equal(t, []string{"ordinary", "partitioned"}, tables)
}

// newDatabasePool connects to a throwaway database of the test's own.
// Adding a member to plpgsql alters an object the whole server shares, so a
// test that does it runs where dropping the database is the only cleanup
// and cannot reach the extension in any other database.
func newDatabasePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, testutil.StartPostgres(t))})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// addToExtensionOrSkip makes table a member of plpgsql, which only the
// extension's owner may do. Containers and the compose database connect as
// superuser; a shared external server that refuses skips the test.
func addToExtensionOrSkip(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), "ALTER EXTENSION plpgsql ADD TABLE "+table)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateInsufficientPrivilege {
		t.Skip("adding a member to plpgsql needs to own the extension; the server refused")
	}
	require.NoError(t, err)
}
