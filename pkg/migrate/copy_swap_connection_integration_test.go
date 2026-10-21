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

// twoDatabases is a pool on one throwaway database and the URL of a second
// on the same server, for a run whose route connection is wired to the
// wrong database. Both databases start empty, so an engine schema in
// either one is proof the run touched it.
type twoDatabases struct {
	pool     *pgxpool.Pool
	poolURL  string
	otherURL string
}

func newTwoDatabases(t *testing.T) twoDatabases {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	poolURL := testutil.NewDatabase(t, serverURL)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: poolURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return twoDatabases{pool: pool, poolURL: poolURL, otherURL: testutil.NewDatabase(t, serverURL)}
}

// createOrders creates the table every run here retypes, in the pool's
// database, and returns its schema.
func (d twoDatabases) createOrders(t *testing.T) string {
	t.Helper()
	schema := testutil.NewSchema(t, d.pool)
	_, err := d.pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id  int PRIMARY KEY,
			qty integer NOT NULL
		)`, schema))
	require.NoError(t, err)
	_, err = d.pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.orders (id, qty)
		SELECT g, g FROM generate_series(1, 100) g`, schema))
	require.NoError(t, err)
	return schema
}

// hasEngineSchema reports whether the database at url carries the engine's
// schema, which the route creates for its checkpoint row before any other
// work.
func hasEngineSchema(t *testing.T, url string) bool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, preflight.EngineSchema).Scan(&exists))
	return exists
}

// columnType reads the declared type of orders.id in the pool's database.
func (d twoDatabases) columnType(t *testing.T, schema string) string {
	t.Helper()
	var typ string
	require.NoError(t, d.pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'id'`, schema).Scan(&typ))
	return typ
}

// A route connection that reaches another database than the pool is a
// wiring mistake, and the route says so before it does anything: Run
// returns the typed mismatch naming both databases and no verdict, and
// neither database carries the engine schema afterwards. Were the run let
// through, the pool's working transactions would find no lock from the
// connection's session and refuse as shadow-lock-unconfirmed — an
// environmental refusal a caller routing on class would retry forever.
// The same statement with the connection on the pool's database is the
// control: it runs to the copy-and-swap outcome.
func TestCopyAndSwapRefusesAConnectionOnAnotherDatabase(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createOrders(t)
	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN id TYPE bigint", schema))

	opts := copySwapRunOptions(d.otherURL)
	v, err := migrate.Run(t.Context(), d.pool, st, opts)
	var mismatch *migrate.CopySwapConnectionMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Zero(t, v, "nothing was executed, so there is no verdict")
	assert.Equal(t, mismatch.PoolCluster, mismatch.ConnectionCluster, "both databases are on the test server")
	assert.NotEqual(t, mismatch.PoolDatabase, mismatch.ConnectionDatabase)
	assert.Equal(t, "integer", d.columnType(t, schema), "the source is untouched")
	assert.False(t, hasEngineSchema(t, d.poolURL), "no checkpoint row was written in the pool's database")
	assert.False(t, hasEngineSchema(t, d.otherURL), "nothing was written through the connection either")

	v, err = migrate.Run(t.Context(), d.pool, st, copySwapRunOptions(d.poolURL))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, "bigint", d.columnType(t, schema))
}

// Desired-state admission reads the same proof before the plan's first
// statement: a plan that routes a retype through copy-and-swap with the
// connection on another database stops as a failed result carrying the
// plan and no verdict, and the native ADD COLUMN ahead of the retype never
// runs.
func TestRunDesiredRefusesAConnectionOnAnotherDatabase(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createOrders(t)

	res, err := migrate.RunDesired(t.Context(), d.pool, migrate.DesiredRequest{
		Schema: schema,
		Desired: parseDesired(t, `CREATE TABLE orders (
			id   bigint PRIMARY KEY,
			qty  integer NOT NULL,
			note text
		)`),
	}, copySwapRunOptions(d.otherURL))
	var mismatch *migrate.CopySwapConnectionMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Empty(t, res.Verdicts, "no statement was attempted")
	assert.Contains(t, res.Detail, "reaches another database than the pool")
	assert.Contains(t, res.Detail, "nothing was executed")
	assert.Equal(t, "integer", d.columnType(t, schema))
	var hasNote bool
	require.NoError(t, d.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'note')`, schema).Scan(&hasNote))
	assert.False(t, hasNote, "the native statement ahead of the retype did not run")
	assert.False(t, hasEngineSchema(t, d.poolURL))
}
