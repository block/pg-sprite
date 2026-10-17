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
	"github.com/block/pg-sprite/pkg/preflight"
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
// door; qualified, the bare name resolves and the run proceeds to the next
// check. The force acknowledgement names another table, so that check is the
// first stage after the resolve and rejects the run before any read that
// needs the resolved table: the only way to reach it is a successful resolve.
func TestRunResolvesAnUnqualifiedTableDespiteAPlantedEqualityOverload(t *testing.T) {
	f := newPlantedEqualityFixture(t, false)

	opts := runOptions()
	opts.Force = f.schema + ".customers"
	v, err := migrate.Run(t.Context(), f.pool,
		parseOne(t, "CREATE INDEX CONCURRENTLY orders_id_idx ON orders (id)"), opts)
	require.Error(t, err)
	assert.NotErrorIs(t, err, preflight.ErrTableNotFound, "the bare name must resolve on the session path")
	assert.Equal(t, verdict.Verdict{}, v)
	assert.False(t, indexExists(t, f.pool, f.schema, "orders_id_idx"), "a mismatched acknowledgement must execute nothing")
}
