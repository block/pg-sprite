package preflight_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// newPlantedOverloadPool returns a pool whose sessions have schema as their
// whole search_path, so pg_catalog is searched implicitly first and schema
// second. That is the path a role gets when its schema is on the path and
// the catalog is not named: an unqualified function or operator whose
// argument types match a schema overload exactly beats the catalog's
// overload that needs a cast, even though the catalog is searched first. It
// is a raw pgx pool on purpose: pkg/dbconn pools carry the session defaults
// the library caller may not have.
func newPlantedOverloadPool(t *testing.T, serverURL, schema string) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(serverURL)
	require.NoError(t, err, "parse server URL")
	pc.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err, "connect with a planted-overload search_path")
	t.Cleanup(pool.Close)
	return pool
}

// plantedSizeFixture is a populated table on a server whose schema holds
// the planted objects a test creates, with an admin pool to create them and
// the pool whose search_path lists that schema.
type plantedSizeFixture struct {
	admin    *pgxpool.Pool
	pool     *pgxpool.Pool
	schema   string
	oracleSz int64
}

func newPlantedSizeFixture(t *testing.T) plantedSizeFixture {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)

	_, err = admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.ledger (
			id bigint PRIMARY KEY,
			note text
		)`, schema))
	require.NoError(t, err)
	_, err = admin.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.ledger (id, note)
		SELECT g, repeat('x', 100) FROM generate_series(1, 10000) AS g`, schema))
	require.NoError(t, err)

	var oracle int64
	require.NoError(t, admin.QueryRow(t.Context(),
		`SELECT pg_catalog.pg_total_relation_size($1::pg_catalog.regclass)`, schema+".ledger").Scan(&oracle))
	require.Positive(t, oracle)

	return plantedSizeFixture{
		admin: admin, pool: newPlantedOverloadPool(t, serverURL, schema), schema: schema, oracleSz: oracle,
	}
}

// requireRefusedAsOversized asserts the size guard still refuses the
// fixture's table against a one-byte limit, reporting the server's real size.
func (f plantedSizeFixture) requireRefusedAsOversized(t *testing.T) {
	t.Helper()
	_, err := preflight.CheckTable(t.Context(), f.pool, f.schema, "ledger", 1)
	var sizeErr *preflight.SizeError
	require.ErrorAs(t, err, &sizeErr)
	assert.Equal(t, f.oracleSz, sizeErr.TotalBytes)
}

// A function planted on the session's search_path whose argument type
// matches the call exactly (oid) is ranked above the catalog's regclass
// signature, which needs a cast. Unqualified, it would report the table as
// 0 bytes and the size guard would admit a table of any size.
func TestCheckTableSizeIgnoresAPlantedFunctionOverload(t *testing.T) {
	f := newPlantedSizeFixture(t)
	_, err := f.admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %s.pg_total_relation_size(oid) RETURNS bigint LANGUAGE sql
		AS 'SELECT 0::bigint'`, f.schema))
	require.NoError(t, err)

	var planted int64
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(
		`SELECT pg_total_relation_size(c.oid) FROM pg_catalog.pg_class c WHERE c.oid = '%s.ledger'::pg_catalog.regclass`,
		f.schema)).Scan(&planted))
	require.Zero(t, planted, "the planted overload must answer an unqualified call on this pool")

	f.requireRefusedAsOversized(t)
}

// The size sum adds a bigint to a numeric; a planted + for exactly that pair
// is ranked above the catalog's numeric + numeric, which needs a cast on the
// left side. Unqualified, the sum would collapse to 0 and the guard would
// admit a table of any size.
func TestCheckTableSizeIgnoresAPlantedOperatorOverload(t *testing.T) {
	f := newPlantedSizeFixture(t)
	for _, ddl := range []string{
		fmt.Sprintf(`
			CREATE FUNCTION %s.zero_sum(bigint, numeric) RETURNS numeric LANGUAGE sql
			AS 'SELECT 0::numeric'`, f.schema),
		fmt.Sprintf(`
			CREATE OPERATOR %s.+ (
				LEFTARG = bigint,
				RIGHTARG = numeric,
				FUNCTION = %s.zero_sum
			)`, f.schema, f.schema),
	} {
		_, err := f.admin.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	var planted int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT 5::bigint + 7::numeric`).Scan(&planted))
	require.Zero(t, planted, "the planted operator must answer an unqualified + on this pool")

	f.requireRefusedAsOversized(t)
}

// The lookup compares pg_class.oid with the regclass to_regclass returns; a
// planted = for exactly (oid, regclass) is ranked above the catalog's
// oid = oid. Unqualified, it would match every pg_class row, so the guard
// would measure and admit whichever relation the server returned first
// instead of the target.
func TestCheckTableIgnoresAPlantedEqualityOverload(t *testing.T) {
	f := newPlantedSizeFixture(t)
	for _, ddl := range []string{
		fmt.Sprintf(`
			CREATE FUNCTION %s.always_equal(oid, regclass) RETURNS boolean LANGUAGE sql
			AS 'SELECT true'`, f.schema),
		fmt.Sprintf(`
			CREATE OPERATOR %s.= (
				LEFTARG = oid,
				RIGHTARG = regclass,
				FUNCTION = %s.always_equal
			)`, f.schema, f.schema),
	} {
		_, err := f.admin.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	var planted bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT 0::oid = '1'::regclass`).Scan(&planted))
	require.True(t, planted, "the planted operator must answer an unqualified = on this pool")

	f.requireRefusedAsOversized(t)
}

// has_schema_privilege(name, name, text) planted on the engine role's
// search_path is ranked above the catalog's (name, text, text). Unqualified,
// it would answer CREATE true for a role the schema never granted it to, and
// the index-build tier would admit it.
func TestCheckPrivilegesIgnoresAPlantedSchemaPrivilegeOverload(t *testing.T) {
	f := newPrivilegeFixture(t)
	f.grant(t, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
		f.schema, pgx.Identifier{f.role}.Sanitize()))
	f.grant(t, fmt.Sprintf("GRANT %s TO %s",
		pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()))
	f.grant(t, fmt.Sprintf(`
		CREATE FUNCTION %s.has_schema_privilege(name, name, text) RETURNS boolean LANGUAGE sql
		AS 'SELECT true'`, f.schema))
	planted := newPlantedOverloadPool(t, f.engineURL, f.schema)

	var answer bool
	require.NoError(t, planted.QueryRow(t.Context(),
		`SELECT has_schema_privilege(current_user, n.nspname, 'CREATE') FROM pg_catalog.pg_namespace n WHERE n.nspname = $1`,
		f.schema).Scan(&answer))
	require.True(t, answer, "the planted overload must answer an unqualified call on this pool")

	_, err := preflight.CheckPrivileges(t.Context(), planted, f.schema, "target", preflight.Requirement{Tier: preflight.TierIndexBuild})
	var privErr *preflight.PrivilegeError
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierIndexBuild, privErr.Tier)
	assert.Equal(t, fmt.Sprintf("GRANT CREATE ON SCHEMA %s TO %s",
		pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{f.owner}.Sanitize()), privErr.Grant)
}

// plantSystemCatalogBlindEquality plants, in schema, an (oid, regclass) =
// that compares OIDs as the catalog does for a user relation but answers
// false for a system catalog. The target lookup still resolves, while every
// comparison against 'pg_catalog.pg_class'::regclass and its siblings
// silently matches nothing. It proves the plant answers an unqualified
// comparison on pool before returning.
func plantSystemCatalogBlindEquality(t *testing.T, admin, pool *pgxpool.Pool, schema string) {
	t.Helper()
	for _, ddl := range []string{
		fmt.Sprintf(`
			CREATE FUNCTION %s.eq_user_only(oid, regclass) RETURNS boolean LANGUAGE sql
			AS 'SELECT $1::pg_catalog.oid OPERATOR(pg_catalog.=) $2::pg_catalog.oid
			       AND $2::pg_catalog.oid OPERATOR(pg_catalog.>=) 16384::pg_catalog.oid'`, schema),
		fmt.Sprintf(`
			CREATE OPERATOR %s.= (
				LEFTARG = oid,
				RIGHTARG = regclass,
				FUNCTION = %s.eq_user_only
			)`, schema, schema),
	} {
		_, err := admin.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	var planted bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid = 'pg_catalog.pg_class'::regclass`).Scan(&planted))
	require.False(t, planted, "the planted operator must answer an unqualified = on this pool")
}

// The owned-name read follows pg_depend edges whose refclassid and classid
// it compares with 'pg_catalog.pg_class'::regclass, an (oid, regclass) pair.
// Unqualified, the planted = would hide every edge, and a serial column's
// sequence would be reported as not owned by the table.
func TestLookupOwnedRelationNamesIgnoresAPlantedRegclassEquality(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)
	_, err = admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id serial PRIMARY KEY
		)`, schema))
	require.NoError(t, err)
	pool := newPlantedOverloadPool(t, serverURL, schema)
	plantSystemCatalogBlindEquality(t, admin, pool, schema)

	names, err := preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	assert.Equal(t, []string{"t_pkey"}, names.ConstraintIndexes)
	assert.Equal(t, []string{"t_id_seq"}, names.Sequences)
}

// The copy-and-swap shape facts count dependent views through pg_rewrite
// edges compared with system-catalog regclass literals. Unqualified, the
// planted = would hide the view, and a table a view selects from would be
// admitted to copy-and-swap.
func TestCheckCopySwapShapeIgnoresAPlantedRegclassEquality(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)
	for _, ddl := range []string{
		fmt.Sprintf(`
			CREATE TABLE %s.viewed (
				id     bigint PRIMARY KEY,
				amount numeric
			)`, schema),
		fmt.Sprintf(`
			CREATE VIEW %s.viewed_totals AS
			SELECT sum(amount) AS total
			FROM %s.viewed`, schema, schema),
	} {
		_, err := admin.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}
	pool := newPlantedOverloadPool(t, serverURL, schema)
	plantSystemCatalogBlindEquality(t, admin, pool, schema)
	role, err := preflight.CheckPrivileges(t.Context(), admin, schema, "viewed", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)

	_, err = preflight.CheckCopySwapShape(t.Context(), pool, schema, "viewed", role)
	requireCopySwapCause(t, err, preflight.CopySwapCauseDependentViews)
}

// The add-column probe runs on the statement's schema then public, and reads
// the probe column back with attrelid = $1::regclass, an (oid, regclass)
// pair. A planted = there that never matches would make the read-back
// return no type and fail the whole proof; qualified, the probe still
// proves a volatile default rewrites the table.
func TestAddColumnRewritesIgnoresAPlantedRegclassEquality(t *testing.T) {
	f := newRewriteFixture(t)
	f.exec(t, `
		CREATE FUNCTION %[1]s.never_equal(oid, regclass) RETURNS boolean LANGUAGE sql
		AS 'SELECT false'`)
	f.exec(t, `
		CREATE OPERATOR %[1]s.= (
			LEFTARG = oid,
			RIGHTARG = regclass,
			FUNCTION = %[1]s.never_equal
		)`)
	planted := newPlantedOverloadPool(t, f.url, f.schema)
	var answer bool
	require.NoError(t, planted.QueryRow(t.Context(),
		`SELECT 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid = 'pg_catalog.pg_class'::regclass`).Scan(&answer))
	require.False(t, answer, "the planted operator must answer an unqualified = on the probe's path")

	proofs, err := preflight.AddColumnRewrites(t.Context(), f.pool, f.schema, "t", fmt.Sprintf(`
		ALTER TABLE %s.t
			ADD COLUMN stamped text DEFAULT clock_timestamp()::text`, f.schema))
	require.NoError(t, err)
	assert.Equal(t, map[string]statement.AddColumnProof{"stamped": statement.AddColumnDefaultRewrites}, proofs)
}
