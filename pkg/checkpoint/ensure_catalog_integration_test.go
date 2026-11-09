package checkpoint_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
)

// decoyCatalog is a schema planted with a lookalike for every catalog name
// Ensure's reads spell: empty pg_namespace, pg_class, and pg_attribute
// relations; pg_get_userbyid, hashtext, quote_ident, and
// pg_advisory_xact_lock functions that record each call in calls and
// answer wrongly; and = on oid, = on name, > on smallint and integer, and
// || on text operators that do the same. Its pool lists the schema ahead of an
// explicit pg_catalog, so an unqualified name in a read resolves to the
// lookalike. The pool is raw on purpose: a pkg/dbconn pool drops that
// shadowing pg_catalog entry, and a library caller's own pool may not.
type decoyCatalog struct {
	schema string
	pool   *pgxpool.Pool
}

func plantDecoyCatalog(t *testing.T, f storeFixture) decoyCatalog {
	t.Helper()
	schema := testutil.NewSchema(t, f.pool)
	for _, ddl := range []string{
		`CREATE TABLE %[1]s.calls (name text NOT NULL)`,
		`
		CREATE TABLE %[1]s.pg_namespace (
			oid      oid,
			nspname  name,
			nspowner oid
		)`,
		`
		CREATE TABLE %[1]s.pg_class (
			oid          oid,
			relname      name,
			relnamespace oid,
			relkind      "char",
			relowner     oid
		)`,
		`
		CREATE TABLE %[1]s.pg_attribute (
			attrelid     oid,
			attname      name,
			attnum       smallint,
			attisdropped boolean
		)`,
		`
		CREATE FUNCTION %[1]s.pg_get_userbyid(oid) RETURNS name LANGUAGE sql
		AS 'INSERT INTO %[1]s.calls VALUES (''pg_get_userbyid''); SELECT ''impostor'''`,
		`
		CREATE FUNCTION %[1]s.hashtext(text) RETURNS integer LANGUAGE sql
		AS 'INSERT INTO %[1]s.calls VALUES (''hashtext''); SELECT 0'`,
		`
		CREATE FUNCTION %[1]s.quote_ident(text) RETURNS text LANGUAGE sql
		AS 'INSERT INTO %[1]s.calls VALUES (''quote_ident''); SELECT ''nope'''`,
		`
		CREATE FUNCTION %[1]s.pg_advisory_xact_lock(integer, integer) RETURNS void LANGUAGE sql
		AS 'INSERT INTO %[1]s.calls VALUES (''pg_advisory_xact_lock'')'`,
		`
		CREATE FUNCTION %[1]s.decoy_oid_eq(oid, oid) RETURNS boolean LANGUAGE sql
		AS 'INSERT INTO %[1]s.calls VALUES (''oid = oid''); SELECT false'`,
		`
		CREATE OPERATOR %[1]s.= (
			LEFTARG  = oid,
			RIGHTARG = oid,
			FUNCTION = %[1]s.decoy_oid_eq
		)`,
		`
		CREATE FUNCTION %[1]s.decoy_name_eq(name, name) RETURNS boolean LANGUAGE sql
		AS 'INSERT INTO %[1]s.calls VALUES (''name = name''); SELECT false'`,
		`
		CREATE OPERATOR %[1]s.= (
			LEFTARG  = name,
			RIGHTARG = name,
			FUNCTION = %[1]s.decoy_name_eq
		)`,
		`
		CREATE FUNCTION %[1]s.decoy_int2_gt_int4(smallint, integer) RETURNS boolean LANGUAGE sql
		AS 'INSERT INTO %[1]s.calls VALUES (''smallint > integer''); SELECT false'`,
		`
		CREATE OPERATOR %[1]s.> (
			LEFTARG  = smallint,
			RIGHTARG = integer,
			FUNCTION = %[1]s.decoy_int2_gt_int4
		)`,
		`
		CREATE FUNCTION %[1]s.decoy_text_cat(text, text) RETURNS text LANGUAGE sql
		AS 'INSERT INTO %[1]s.calls VALUES (''text || text''); SELECT '''''`,
		`
		CREATE OPERATOR %[1]s.|| (
			LEFTARG  = text,
			RIGHTARG = text,
			FUNCTION = %[1]s.decoy_text_cat
		)`,
	} {
		_, err := f.pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err)
	}
	pool := testutil.NewCatalogShadowingPool(t, f.url, schema)
	return decoyCatalog{schema: schema, pool: pool}
}

// callsRecorded is how many times any lookalike function or operator was
// consulted since the schema was planted or last cleared.
func (d decoyCatalog) callsRecorded(t *testing.T) int {
	t.Helper()
	var calls int
	require.NoError(t, d.pool.QueryRow(t.Context(),
		fmt.Sprintf(`SELECT count(*) FROM %s.calls`, d.schema)).Scan(&calls))
	return calls
}

func (d decoyCatalog) clearCalls(t *testing.T) {
	t.Helper()
	_, err := d.pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %s.calls`, d.schema))
	require.NoError(t, err)
}

// Every bare name Ensure's reads use resolves to a lookalike on this
// pool, so the lookalikes are what an unqualified read would consult.
func (d decoyCatalog) assertShadows(t *testing.T) {
	t.Helper()
	var namespaces int
	require.NoError(t, d.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_namespace`).Scan(&namespaces))
	require.Zero(t, namespaces, "the lookalike pg_namespace must shadow the catalog on this pool")
	var owner string
	require.NoError(t, d.pool.QueryRow(t.Context(), `SELECT pg_get_userbyid(10)`).Scan(&owner))
	require.Equal(t, "impostor", owner, "the lookalike pg_get_userbyid must shadow the catalog on this pool")
	var equal bool
	require.NoError(t, d.pool.QueryRow(t.Context(), `SELECT 1::oid = 1::oid`).Scan(&equal))
	require.False(t, equal, "the lookalike oid = oid must shadow the catalog on this pool")
	require.Equal(t, 2, d.callsRecorded(t), "the lookalike function and operator each record their call")
	d.clearCalls(t)
}

// Ensure on a pool whose search_path lists a schema of lookalikes ahead of
// pg_catalog still reads the real catalog: it finds the engine's own
// schema and table, owned by this role, with today's shape, and consults
// no lookalike. Were any read unqualified, the empty pg_namespace would
// report the schema absent and the create would collide with it, the
// pg_get_userbyid lookalike would name an owner that is not this role, or
// the empty pg_attribute would report every column missing. The table is
// created beforehand through the engine's own pool, so what runs under the
// shadowing path is the read-and-judge half of Ensure, not its DDL.
func TestEnsureReadsResolveToTheRealCatalog(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	shape := f.columnNames(t)

	decoys := plantDecoyCatalog(t, f)
	decoys.assertShadows(t)
	store, err := checkpoint.NewStore(decoys.pool, checkpoint.Options{Clock: f.clock})
	require.NoError(t, err)

	err = store.Ensure(t.Context())

	assert.NoError(t, err, "the engine's own table is recognised as its own under the shadowing path")
	assert.Zero(t, decoys.callsRecorded(t), "no lookalike function or operator is consulted")
	assert.Equal(t, shape, f.columnNames(t), "the table keeps its shape; nothing was judged missing")
}
