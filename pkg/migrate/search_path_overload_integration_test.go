package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// plantedEqualityFixture is a schema holding two tables, an index on one of
// them, and a planted oid = regclass operator that answers every comparison
// with one fixed boolean, with a pool whose whole search_path is that schema.
//
// With only the schema on the path, pg_catalog is searched implicitly first
// and the schema second. An unqualified oid = regclass comparison still
// resolves to the planted operator, because its argument types match exactly
// where the catalog's oid = oid needs a cast. Answering true makes an
// unqualified catalog lookup by regclass match every pg_class row; answering
// false makes it match none. The pool is a raw pgx pool on purpose:
// pkg/dbconn pools carry session defaults a library caller may not have.
type plantedEqualityFixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newPlantedEqualityFixture(t *testing.T, answer bool) plantedEqualityFixture {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)

	for _, ddl := range []string{
		fmt.Sprintf(`
			CREATE TABLE %[1]s.orders (
				id int PRIMARY KEY,
				created_at timestamptz
			)`, schema),
		fmt.Sprintf(`
			CREATE TABLE %[1]s.customers (
				id int PRIMARY KEY
			)`, schema),
		fmt.Sprintf(`CREATE INDEX orders_created_idx ON %[1]s.orders (created_at)`, schema),
		fmt.Sprintf(`
			CREATE FUNCTION %[1]s.planted_equal(oid, regclass) RETURNS boolean LANGUAGE sql
			AS 'SELECT %[2]t'`, schema, answer),
		fmt.Sprintf(`
			CREATE OPERATOR %[1]s.= (
				LEFTARG = oid,
				RIGHTARG = regclass,
				FUNCTION = %[1]s.planted_equal
			)`, schema),
	} {
		_, err := admin.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	pc, err := pgxpool.ParseConfig(serverURL)
	require.NoError(t, err, "parse server URL")
	pc.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err, "connect with a planted-overload search_path")
	t.Cleanup(pool.Close)

	var planted bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT 0::oid = '1'::regclass`).Scan(&planted))
	require.Equal(t, answer, planted, "the planted operator must answer an unqualified = on this pool")

	return plantedEqualityFixture{pool: pool, schema: schema}
}

// The acknowledgement gate looks up the table an index statement locks with
// c.oid = to_regclass(...). Unqualified, the planted oid = regclass matches
// every pg_class row, so the gate would read whichever relation the server
// returned first instead of the named index: the acknowledgement would be
// checked against the wrong table, or the accepted statement refused for a
// relation that is not the one it names. The gate's answer is the same as on
// an unplanted path: the owning table is found and the statement runs.
func TestRunAcceptBlockingIgnoresAPlantedEqualityOverload(t *testing.T) {
	f := newPlantedEqualityFixture(t, true)

	v, err := migrate.Run(t.Context(), f.pool,
		parseOne(t, fmt.Sprintf("DROP INDEX %s.orders_created_idx", f.schema)),
		acceptBlockingOptions(f.schema+".orders"))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, v.Outcome)
	assert.Equal(t, f.schema+".orders", v.Table)
	assert.False(t, indexExists(t, f.pool, f.schema, "orders_created_idx"), "the accepted DROP INDEX must have committed")
}

// The gate still rejects an acknowledgement that names a table the statement
// does not lock: the owner is read from the real catalog, so the mismatch is
// found and nothing executes.
func TestRunAcceptBlockingStillRejectsAFalseAcknowledgementUnderAPlantedEqualityOverload(t *testing.T) {
	f := newPlantedEqualityFixture(t, true)

	v, err := migrate.Run(t.Context(), f.pool,
		parseOne(t, fmt.Sprintf("DROP INDEX %s.orders_created_idx", f.schema)),
		acceptBlockingOptions(f.schema+".customers"))
	require.ErrorIs(t, err, migrate.ErrAcceptBlockingMismatch)
	assert.Equal(t, verdict.Verdict{}, v)
	assert.True(t, indexExists(t, f.pool, f.schema, "orders_created_idx"), "a mismatched acknowledgement must execute nothing")
}

// An unqualified table is resolved once against the session's search_path
// with c.oid = to_regclass(...). Unqualified, a planted oid = regclass that
// answers false matches no pg_class row, so the table that is on the path
// would be reported as not visible and the statement refused at the front
// door; one that answers true matches every row, so the name could resolve
// to whichever schema the server returned first. Qualified, the bare name
// resolves to the table on the path either way, and the whole run reaches
// the same verdict as on an unplanted path: the index is built on that
// table.
func TestRunResolvesAnUnqualifiedTableDespiteAPlantedEqualityOverload(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(fmt.Sprintf("planted operator answers %t", answer), func(t *testing.T) {
			f := newPlantedEqualityFixture(t, answer)

			v, err := migrate.Run(t.Context(), f.pool,
				parseOne(t, "CREATE INDEX CONCURRENTLY orders_id_idx ON orders (id)"), runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
			assert.Equal(t, f.schema+".orders", v.Table, "the bare name resolves to the table on the session path")
			assert.True(t, indexExists(t, f.pool, f.schema, "orders_id_idx"))
		})
	}
}

// newShadowedCatalogFixture is a schema holding the same two tables and
// index, plus decoys for every catalog name the front door's two reads
// use: pg_class, pg_namespace, and pg_index relations, to_regclass and
// quote_ident functions, an oid = oid operator that is never true, and a
// text domain that rejects every value. Its pool lists the schema ahead
// of an explicit pg_catalog, so an unqualified name in a read resolves to
// a decoy. The pool is raw on purpose: a pkg/dbconn pool drops that
// shadowing pg_catalog entry, and a library caller's own pool may not.
func newShadowedCatalogFixture(t *testing.T) plantedEqualityFixture {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)

	for _, ddl := range []string{
		fmt.Sprintf(`
			CREATE TABLE %[1]s.orders (
				id int PRIMARY KEY,
				created_at timestamptz
			)`, schema),
		fmt.Sprintf(`
			CREATE TABLE %[1]s.customers (
				id int PRIMARY KEY
			)`, schema),
		fmt.Sprintf(`CREATE INDEX orders_created_idx ON %[1]s.orders (created_at)`, schema),
		fmt.Sprintf(`
			CREATE TABLE %[1]s.pg_class (
				oid oid,
				relname name,
				relnamespace oid,
				relkind "char"
			)`, schema),
		fmt.Sprintf(`
			CREATE TABLE %[1]s.pg_namespace (
				oid oid,
				nspname name
			)`, schema),
		fmt.Sprintf(`
			CREATE TABLE %[1]s.pg_index (
				indexrelid oid,
				indrelid oid
			)`, schema),
		fmt.Sprintf(`
			CREATE FUNCTION %[1]s.to_regclass(text) RETURNS regclass LANGUAGE sql
			AS 'SELECT NULL::regclass'`, schema),
		fmt.Sprintf(`
			CREATE FUNCTION %[1]s.quote_ident(text) RETURNS text LANGUAGE sql
			AS 'SELECT ''nope'''`, schema),
		fmt.Sprintf(`
			CREATE FUNCTION %[1]s.decoy_oid_eq(oid, oid) RETURNS boolean LANGUAGE sql
			AS 'SELECT false'`, schema),
		fmt.Sprintf(`
			CREATE OPERATOR %[1]s.= (
				LEFTARG = oid,
				RIGHTARG = oid,
				FUNCTION = %[1]s.decoy_oid_eq
			)`, schema),
		fmt.Sprintf(`CREATE DOMAIN %[1]s.text AS pg_catalog.text CHECK (false)`, schema),
	} {
		_, err := admin.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	pool := testutil.NewCatalogShadowingPool(t, serverURL, schema)
	var rows int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_class`).Scan(&rows))
	require.Zero(t, rows, "the decoy pg_class must shadow the catalog on this pool")

	return plantedEqualityFixture{pool: pool, schema: schema}
}

// The target resolve reads pg_class and pg_namespace, calls to_regclass
// and quote_ident, and compares oids. With every one of those names
// shadowed by a decoy that answers wrongly, the bare name still resolves
// to the table on the session path and the run builds the index on it.
func TestRunResolvesAnUnqualifiedTableUnderAShadowedCatalog(t *testing.T) {
	f := newShadowedCatalogFixture(t)

	v, err := migrate.Run(t.Context(), f.pool,
		parseOne(t, "CREATE INDEX CONCURRENTLY orders_id_idx ON orders (id)"), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, f.schema+".orders", v.Table)
	assert.True(t, indexExists(t, f.pool, f.schema, "orders_id_idx"))
}

// The acknowledgement gate's lookup reads pg_index, pg_class, and
// pg_namespace, calls to_regclass, compares oids, and casts to text. With
// every one of those names shadowed, it still finds orders as the table
// the index belongs to, so an acknowledgement naming customers is the
// typed mismatch and nothing executes. A decoy answering any part of the
// lookup would find no table at all, which is a different refusal. The
// mismatch direction stops at the gate, before the executor's own catalog
// reads, which this fixture does not cover.
func TestRunAcceptBlockingReadsTheRealCatalogUnderAShadowedCatalog(t *testing.T) {
	f := newShadowedCatalogFixture(t)

	v, err := migrate.Run(t.Context(), f.pool,
		parseOne(t, fmt.Sprintf("DROP INDEX %s.orders_created_idx", f.schema)),
		acceptBlockingOptions(f.schema+".customers"))
	require.ErrorIs(t, err, migrate.ErrAcceptBlockingMismatch)
	assert.Equal(t, verdict.Verdict{}, v)
	assert.True(t, indexExists(t, f.pool, f.schema, "orders_created_idx"), "a mismatched acknowledgement must execute nothing")
}

// The other direction: an acknowledgement naming orders passes the gate,
// because the lookup follows the index through pg_index to its table.
// Were any of the joins that reach the table answered by a decoy, the
// lookup would name the index itself and the acknowledgement would be a
// mismatch. Past the gate the executor runs its own catalog reads, which
// this fixture does not cover, so the test reads only what the gate
// decided: every verdict after the gate records the table it resolved.
func TestRunAcceptBlockingAcceptsTheLockedTableUnderAShadowedCatalog(t *testing.T) {
	f := newShadowedCatalogFixture(t)

	v, err := migrate.Run(t.Context(), f.pool,
		parseOne(t, fmt.Sprintf("DROP INDEX %s.orders_created_idx", f.schema)),
		acceptBlockingOptions(f.schema+".orders"))
	assert.NotErrorIs(t, err, migrate.ErrAcceptBlockingMismatch)
	assert.Equal(t, f.schema+".orders", v.Table, "the gate resolved the table the index belongs to")
}
