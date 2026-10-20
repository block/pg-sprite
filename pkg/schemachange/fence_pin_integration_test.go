package schemachange

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// fencePinFixture is a database, a schema, and a library caller's own pool
// over the same server with the schema ahead of pg_catalog, for the fence
// transactions the Runner drives on the caller's pool and no exported
// function reaches.
type fencePinFixture struct {
	cfg       dbconn.Config
	pool      *pgxpool.Pool
	shadowing *pgxpool.Pool
	schema    string
}

func newFencePinFixture(t *testing.T) fencePinFixture {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{preflight.EngineSchema}.Sanitize())
	require.NoError(t, err, "the fence function lives in the engine's schema")
	return fencePinFixture{
		cfg:       cfg,
		pool:      pool,
		shadowing: testutil.NewCatalogShadowingPool(t, cfg.URL, schema),
		schema:    schema,
	}
}

func (f fencePinFixture) exec(t *testing.T, ddl string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(ddl, "%s", f.schema))
	require.NoError(t, err)
}

func (f fencePinFixture) prove(t *testing.T, table string) preflight.CopySwapTarget {
	t.Helper()
	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.schema, table, preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	return target
}

func (f fencePinFixture) lock(t *testing.T, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table)
	require.NoError(t, err)
	t.Cleanup(func() {
		if lock.Err() == nil {
			assert.NoError(t, lock.Release(context.WithoutCancel(t.Context())))
		}
	})
	return lock
}

// plantNeverEqual puts an operator `=` over typ in the schema that is never
// true, so an unqualified comparison of that type resolved through the
// schema finds no match.
func (f fencePinFixture) plantNeverEqual(t *testing.T, typ string) {
	t.Helper()
	fn := "never_equal_" + strings.Trim(typ, `"`)
	f.exec(t, `
		CREATE FUNCTION %s.`+fn+`(`+typ+`, `+typ+`) RETURNS boolean
		LANGUAGE sql IMMUTABLE
		AS 'SELECT false'`)
	f.exec(t, `CREATE OPERATOR %s.= (LEFTARG = `+typ+`, RIGHTARG = `+typ+`, FUNCTION = %s.`+fn+`)`)
	var equal bool
	require.NoError(t, f.shadowing.QueryRow(t.Context(), `SELECT 'x'::`+typ+` = 'x'::`+typ).Scan(&equal))
	require.False(t, equal, "the impostor %s equality answers an unqualified comparison on the shadowing pool", typ)
}

// fenceTriggerCount counts triggers of the fence's name on schema.table,
// read on the fixture's own catalog-first pool.
func (f fencePinFixture) fenceTriggerCount(t *testing.T, table string) int {
	t.Helper()
	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM pg_trigger tg
		JOIN pg_class c ON c.oid = tg.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND tg.tgname = $3 AND NOT tg.tgisinternal`,
		f.schema, table, WriteFenceTrigger).Scan(&count))
	return count
}

// The fence install runs on the caller's pool, so its answer must not depend
// on that pool's session search_path (CO-9). The pool here has the table's
// schema ahead of pg_catalog, an impostor hashtext in that schema that
// raises, which an unqualified lock-key read would call, and an impostor
// text equality that is never true, which an unqualified lock confirmation
// would use. The install must take the fence's advisory key, confirm the
// lock the session holds, and put the fence on the source.
func TestInstallWriteFenceIgnoresTheSessionSearchPath(t *testing.T) {
	f := newFencePinFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	f.exec(t, `
		CREATE FUNCTION %s.hashtext(text) RETURNS integer
		LANGUAGE plpgsql IMMUTABLE
		AS $$ BEGIN RAISE EXCEPTION 'impostor hashtext'; END $$`)
	var key int
	err := f.shadowing.QueryRow(t.Context(), `SELECT hashtext('orders')`).Scan(&key)
	require.Error(t, err, "the impostor hashtext answers an unqualified call on the shadowing pool")
	f.plantNeverEqual(t, "text")

	require.NoError(t, installWriteFence(t.Context(), f.shadowing, lock, target, Options{}))

	assert.Equal(t, 1, f.fenceTriggerCount(t, "orders"), "the fence is on the source")
}

// Under KeepOld the fence rides the rename and comes off the retained table
// in a transaction on the caller's pool, which confirms the lock through
// pkg/dbconn and resolves the old name's OID against the swap proof (ST-6).
// The pool here has the schema ahead of pg_catalog, with impostor name and
// text equalities that are never true; on the caller's path the OID read
// would find no relation and the lock confirmation no holder. The fence
// transaction pins its search_path to the catalog, so the retained table is
// recognized and the fence comes off it.
func TestDropWriteFenceFromOldIgnoresTheSessionSearchPath(t *testing.T) {
	f := newFencePinFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	require.NoError(t, installWriteFence(t.Context(), f.pool, lock, target, Options{}))
	old := OldName(f.schema, "orders")
	var oldOID uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT $1::regclass::oid`, pgx.Identifier{f.schema, "orders"}.Sanitize()).Scan(&oldOID))
	f.exec(t, `ALTER TABLE %s.orders RENAME TO `+pgx.Identifier{old}.Sanitize())
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty bigint NOT NULL
		)`)
	require.Equal(t, 1, f.fenceTriggerCount(t, old), "the fence rode the rename")
	f.plantNeverEqual(t, "name")
	f.plantNeverEqual(t, "text")
	swapped := SwappedTable{schema: f.schema, table: "orders", oldOID: oldOID}

	require.NoError(t, dropWriteFenceFromOld(t.Context(), f.shadowing, lock, swapped, Options{}))

	assert.Equal(t, 0, f.fenceTriggerCount(t, old), "the fence is off the retained table")
}
