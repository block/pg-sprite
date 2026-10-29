package migrate_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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
// through, its table lock would be an advisory lock in the other database,
// which does not keep a second run wired to the pool's database off the
// table. The same statement with the connection on the pool's database is the
// control: it runs to the copy-and-swap outcome.
func TestCopyAndSwapRefusesAConnectionOnAnotherDatabase(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createOrders(t)
	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN id TYPE bigint", schema))

	opts := copySwapRunOptions(d.otherURL, schema+".orders")
	v, err := migrate.Run(t.Context(), d.pool, st, opts)
	var mismatch *migrate.CopySwapConnectionMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Zero(t, v, "nothing was executed, so there is no verdict")
	assert.Equal(t, mismatch.PoolCluster, mismatch.ConnectionCluster, "both databases are on the test server")
	assert.NotEqual(t, mismatch.PoolDatabase, mismatch.ConnectionDatabase)
	assert.Equal(t, "integer", d.columnType(t, schema), "the source is untouched")
	assert.False(t, hasEngineSchema(t, d.poolURL), "no checkpoint row was written in the pool's database")
	assert.False(t, hasEngineSchema(t, d.otherURL), "nothing was written through the connection either")

	v, err = migrate.Run(t.Context(), d.pool, st, copySwapRunOptions(d.poolURL, schema+".orders"))
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
	}, copySwapRunOptions(d.otherURL, schema+".orders"))
	var mismatch *migrate.CopySwapConnectionMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, mismatch.PoolCluster, mismatch.ConnectionCluster, "both databases are on the test server")
	assert.NotEqual(t, mismatch.PoolDatabase, mismatch.ConnectionDatabase)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Empty(t, res.Reason, "a stop is not a refusal")
	assert.NotEmpty(t, res.Plan.Statements, "the stop carries the plan it did not run")
	assert.Empty(t, res.Verdicts, "no statement was attempted")
	assert.Equal(t, "integer", d.columnType(t, schema))
	var hasNote bool
	require.NoError(t, d.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'note')`, schema).Scan(&hasNote))
	assert.False(t, hasNote, "the native statement ahead of the retype did not run")
	assert.False(t, hasEngineSchema(t, d.poolURL))
}

// A connection on a database of the pool's name but on another cluster is
// the same wiring mistake: the database names agree, so only the clusters'
// system identifiers tell the two apart. The second server is a container
// of its own, since StartPostgres may hand back the one shared server.
func TestCopyAndSwapRefusesADatabaseOfTheSameNameOnAnotherCluster(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createOrders(t)
	name := databaseName(t, d.poolURL)
	otherServer := testutil.StartPostgresWithSettings(t)
	admin, err := pgxpool.New(t.Context(), otherServer)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	sameName := withDatabase(t, otherServer, name)

	v, err := migrate.Run(t.Context(), d.pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN id TYPE bigint", schema)),
		copySwapRunOptions(sameName, schema+".orders"))
	var mismatch *migrate.CopySwapConnectionMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Zero(t, v, "nothing was executed, so there is no verdict")
	assert.Equal(t, name, mismatch.PoolDatabase)
	assert.Equal(t, name, mismatch.ConnectionDatabase)
	assert.NotEqual(t, mismatch.PoolCluster, mismatch.ConnectionCluster, "two servers, two system identifiers")
	assert.Equal(t, "integer", d.columnType(t, schema), "the source is untouched")
	assert.False(t, hasEngineSchema(t, d.poolURL), "no checkpoint row was written in the pool's database")
	assert.False(t, hasEngineSchema(t, sameName), "nothing was written through the connection either")
}

// A connection that cannot be opened proves nothing about where the route's
// sessions would land, so both front doors stop before anything runs: no
// verdict from Run, no statement attempted by RunDesired, and the error is
// not the mismatch, since there is no second identity to compare.
func TestCopyAndSwapStopsWhenTheConnectionCannotBeOpened(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createOrders(t)
	missing := withDatabase(t, d.poolURL, "no_such_database")

	assertStopsBeforeAnyStatement(t, d, d.pool, schema, copySwapRunOptions(missing, schema+".orders"))
}

// The identity is read with pg_control_system(), which the server grants to
// PUBLIC; revoked in the pool's database, a role without it cannot be
// identified. Whichever side that role is on, the check fails closed: the
// run stops before anything executes rather than assuming the two sides
// agree. The superuser on the other side keeps its access.
func TestCopyAndSwapStopsWhenEitherSideCannotBeIdentified(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createOrders(t)
	_, err := d.pool.Exec(t.Context(), `
		REVOKE EXECUTE ON FUNCTION pg_catalog.pg_control_system()
		FROM PUBLIC`)
	require.NoError(t, err)
	role := testutil.NewRole(t, d.pool, "LOGIN PASSWORD 'unidentified'")
	// A session that does not own the function, as on a managed server whose
	// test user is not superuser, revokes nothing and is only warned, so the
	// role can still be identified and there is nothing to test.
	var identifiable bool
	require.NoError(t, d.pool.QueryRow(t.Context(), `
		SELECT pg_catalog.has_function_privilege($1, 'pg_catalog.pg_control_system()', 'EXECUTE')`,
		role).Scan(&identifiable))
	if identifiable {
		t.Skip("this session cannot revoke pg_control_system() from PUBLIC")
	}
	t.Cleanup(func() {
		// The role owns the table by then; its drop needs nothing left
		// behind. Cleanups run in reverse, so this runs before the drop.
		_, err := d.pool.Exec(context.WithoutCancel(t.Context()),
			"DROP OWNED BY "+pgx.Identifier{role}.Sanitize())
		assert.NoError(t, err)
	})
	// The role owns the table and may create the scratch schema desired
	// planning reads into, so planning gets as far as the check.
	_, err = d.pool.Exec(t.Context(), fmt.Sprintf(`
		GRANT CREATE ON DATABASE %s
		TO %s`, pgx.Identifier{databaseName(t, d.poolURL)}.Sanitize(), pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)
	_, err = d.pool.Exec(t.Context(), fmt.Sprintf(`
		GRANT USAGE ON SCHEMA %s
		TO %s`, schema, pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)
	_, err = d.pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %s.orders
		OWNER TO %s`, schema, pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)
	roleURL := withUser(t, d.poolURL, role, "unidentified")

	t.Run("the connection", func(t *testing.T) {
		assertStopsBeforeAnyStatement(t, d, d.pool, schema, copySwapRunOptions(roleURL, schema+".orders"))
	})

	t.Run("the pool", func(t *testing.T) {
		rolePool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: roleURL})
		require.NoError(t, err)
		t.Cleanup(rolePool.Close)
		assertStopsBeforeAnyStatement(t, d, rolePool, schema, copySwapRunOptions(d.poolURL, schema+".orders"))
	})
}

// assertStopsBeforeAnyStatement runs the retype through Run and, as a
// desired schema that also adds a column natively ahead of the retype,
// through RunDesired, both on runPool with opts, and asserts that each stops
// with an error other than the mismatch and that nothing was executed: no
// verdict, no statement attempted, the source and the native column
// untouched, and no engine schema in the pool's database.
func assertStopsBeforeAnyStatement(t *testing.T, d twoDatabases, runPool *pgxpool.Pool, schema string, opts migrate.Options) {
	t.Helper()
	var mismatch *migrate.CopySwapConnectionMismatchError

	v, err := migrate.Run(t.Context(), runPool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN id TYPE bigint", schema)), opts)
	require.Error(t, err)
	assert.NotErrorAs(t, err, &mismatch)
	assert.Zero(t, v, "nothing was executed, so there is no verdict")

	res, err := migrate.RunDesired(t.Context(), runPool, migrate.DesiredRequest{
		Schema: schema,
		Desired: parseDesired(t, `CREATE TABLE orders (
			id   bigint PRIMARY KEY,
			qty  integer NOT NULL,
			note text
		)`),
	}, opts)
	require.Error(t, err)
	assert.NotErrorAs(t, err, &mismatch)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Empty(t, res.Verdicts, "no statement was attempted")

	assert.Equal(t, "integer", d.columnType(t, schema), "the source is untouched")
	var hasNote bool
	require.NoError(t, d.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'note')`, schema).Scan(&hasNote))
	assert.False(t, hasNote, "the native statement ahead of the retype did not run")
	assert.False(t, hasEngineSchema(t, d.poolURL), "no checkpoint row was written")
}

// databaseName returns the database a URL-form DSN names.
func databaseName(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	name := strings.TrimPrefix(u.Path, "/")
	require.NotEmpty(t, name, "the DSN names no database")
	return name
}

// withDatabase returns a URL-form DSN with its database replaced by name.
func withDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

// withUser returns a URL-form DSN that logs in as user with password.
func withUser(t *testing.T, dsn, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(user, password)
	return u.String()
}
