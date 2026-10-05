package checkpoint_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// storeFixture is a store over a throwaway database of its own: the
// checkpoint table is one per database, so a fresh database is what makes
// "first use" observable.
type storeFixture struct {
	url   string
	pool  *pgxpool.Pool
	store *checkpoint.Store
	clock *fixedClock
}

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

var stampedAt = time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)

func newStoreFixture(t *testing.T) storeFixture {
	t.Helper()
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	clock := &fixedClock{now: stampedAt}
	store, err := checkpoint.NewStore(pool, checkpoint.Options{Clock: clock})
	require.NoError(t, err)
	return storeFixture{url: url, pool: pool, store: store, clock: clock}
}

// ensure creates the table and asserts it is there.
func (f storeFixture) ensure(t *testing.T) {
	t.Helper()
	require.NoError(t, f.store.Ensure(t.Context()))
	assert.True(t, f.tableExists(t))
}

func (f storeFixture) tableExists(t *testing.T) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT to_regclass($1) IS NOT NULL", checkpoint.SchemaName+"."+checkpoint.TableName).Scan(&exists))
	return exists
}

// lock takes the per-table lock session every checkpoint write requires
// and releases it when the test ends.
func (f storeFixture) lock(t *testing.T, schema, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: f.url}, schema, table)
	require.NoError(t, err)
	t.Cleanup(func() {
		// A test that deliberately loses the lock has already seen Release's
		// invariant error through Err; a clean test releases cleanly.
		if lock.Err() == nil {
			assert.NoError(t, lock.Release(context.WithoutCancel(t.Context())))
		}
	})
	return lock
}

// ordersLock is the lock session for the app.orders target every test here
// checkpoints.
func (f storeFixture) ordersLock(t *testing.T) *dbconn.TableLockSession {
	t.Helper()
	return f.lock(t, "app", "orders")
}

func (f storeFixture) rowCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM pgsprite.pgsprite_checkpoint").Scan(&n))
	return n
}

// exec runs SQL as the fixture's superuser.
func (f storeFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), sql)
	require.NoError(t, err)
}

// rolePassword is the password every throwaway login role here is created
// with.
const rolePassword = "checkpoint-test-password"

// roleURL is the fixture database's URL with role as the user.
func (f storeFixture) roleURL(t *testing.T, role string) string {
	t.Helper()
	u, err := url.Parse(f.url)
	require.NoError(t, err, "parse database URL")
	u.User = url.UserPassword(role, rolePassword)
	return u.String()
}

// engineRole creates a login role with no privilege beyond CONNECT — the
// least an engine role can be — and a store over a pool connected as it.
func (f storeFixture) engineRole(t *testing.T) (string, *checkpoint.Store) {
	t.Helper()
	role := testutil.NewRole(t, f.pool, "LOGIN PASSWORD '"+rolePassword+"'")
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: f.roleURL(t, role)})
	require.NoError(t, err, "connect as %s", role)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{Clock: f.clock})
	require.NoError(t, err)
	return role, store
}

// ordersCheckpoint is the record a copy of app.orders stopped at key 1000
// would save: a quiesced run, so no slot and no publication.
func ordersCheckpoint() checkpoint.Checkpoint {
	return checkpoint.Checkpoint{
		Schema:            "app",
		Table:             "orders",
		ShadowTable:       "_pgsprite_orders_new",
		Watermark:         copier.NewWatermark(1000),
		LastAppliedLSN:    decode.LSN(0x00000016b374d848),
		SourceFingerprint: "src-a",
		TargetFingerprint: "tgt-a",
		Phase:             checkpoint.PhaseCopying,
	}
}

// Ensure creates the engine schema and the table on first use and is a
// no-op afterwards; nothing else is needed before the first Save.
func TestEnsureCreatesTheTableOnceAndIsIdempotent(t *testing.T) {
	f := newStoreFixture(t)
	assert.False(t, f.tableExists(t), "a fresh database has no checkpoint table")
	f.ensure(t)
	f.ensure(t)
	assert.Equal(t, int64(0), f.rowCount(t))
}

// A second engine's Ensure that arrives while the first engine's create is
// still uncommitted waits on the engine's advisory key, then finds the
// schema committed and succeeds. Without the key its CREATE SCHEMA would
// race the uncommitted one into a duplicate-key failure in pg_namespace.
func TestEnsureWaitsForAConcurrentFirstCreate(t *testing.T) {
	f := newStoreFixture(t)
	first, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Rollback(context.WithoutCancel(t.Context())) })
	_, err = first.Exec(t.Context(), "SELECT pg_advisory_xact_lock($1, hashtext(quote_ident($2) || '.' || quote_ident($3)))",
		dbconn.TableLockClassID, checkpoint.SchemaName, checkpoint.TableName)
	require.NoError(t, err)
	_, err = first.Exec(t.Context(), `CREATE SCHEMA "pgsprite"`)
	require.NoError(t, err)

	second := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { second <- f.store.Ensure(t.Context()) })
	t.Cleanup(wg.Wait)
	const waitingDeadline = 5 * time.Second
	require.Eventually(t, func() bool {
		var waiting bool
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE NOT granted)").Scan(&waiting))
		return waiting
	}, waitingDeadline, 10*time.Millisecond, "the second Ensure should be waiting behind the first create")
	require.NoError(t, first.Commit(t.Context()))
	const ensureDeadline = 5 * time.Second
	select {
	case err := <-second:
		require.NoError(t, err)
	case <-time.After(ensureDeadline):
		t.Fatalf("the second Ensure did not finish within %s of the first commit", ensureDeadline)
	}
	assert.True(t, f.tableExists(t))
}

// A schema and table the engine role owns but did not create itself — a
// DBA pre-provisioned them so the engine never needs CREATE on the
// database — are accepted as they are: Ensure creates nothing, and Save
// lands. The engine role here has LOGIN and nothing else, so the CREATE
// SCHEMA that the privilege check would refuse is never issued.
func TestEnsureAcceptsAPreProvisionedTableWithoutDatabaseCreate(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	engine, engineStore := f.engineRole(t)
	f.exec(t, `ALTER SCHEMA "pgsprite" OWNER TO `+pgx.Identifier{engine}.Sanitize())
	f.exec(t, `ALTER TABLE "pgsprite"."pgsprite_checkpoint" OWNER TO `+pgx.Identifier{engine}.Sanitize())
	var canCreate bool
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT has_database_privilege($1, current_database(), 'CREATE')", engine).Scan(&canCreate))
	require.False(t, canCreate, "the engine role must lack database CREATE for this test to mean anything")

	require.NoError(t, engineStore.Ensure(t.Context()))
	lock, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: f.roleURL(t, engine)}, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, lock.Release(context.WithoutCancel(t.Context()))) })
	require.NoError(t, engineStore.Save(t.Context(), lock, ordersCheckpoint()))
	assert.Equal(t, int64(1), f.rowCount(t))
}

// An engine schema another role owns is refused, typed, rather than
// adopted: whatever that role hangs off the schema or its table would run
// with the engine's privileges on every Save.
func TestEnsureRefusesASchemaAnotherRoleOwns(t *testing.T) {
	f := newStoreFixture(t)
	other := testutil.NewRole(t, f.pool, "NOLOGIN")
	f.exec(t, `CREATE SCHEMA "pgsprite" AUTHORIZATION `+pgx.Identifier{other}.Sanitize())

	err := f.store.Ensure(t.Context())
	assert.ErrorIs(t, err, checkpoint.ErrForeignObject)
	assert.False(t, f.tableExists(t), "nothing is created inside a foreign schema")
}

// A checkpoint table another role owns is refused the same way, even when
// the engine owns the schema around it.
func TestEnsureRefusesATableAnotherRoleOwns(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	other := testutil.NewRole(t, f.pool, "NOLOGIN")
	f.exec(t, `ALTER TABLE "pgsprite"."pgsprite_checkpoint" OWNER TO `+pgx.Identifier{other}.Sanitize())

	err := f.store.Ensure(t.Context())
	assert.ErrorIs(t, err, checkpoint.ErrForeignObject)
}

// A view wearing the checkpoint table's name is not the checkpoint table,
// whoever owns it: only a plain table is accepted.
func TestEnsureRefusesAViewUnderTheTableName(t *testing.T) {
	f := newStoreFixture(t)
	f.exec(t, `CREATE SCHEMA "pgsprite"`)
	f.exec(t, `CREATE VIEW "pgsprite"."pgsprite_checkpoint" AS SELECT 1 AS schema_name`)

	err := f.store.Ensure(t.Context())
	assert.ErrorIs(t, err, checkpoint.ErrForeignObject)
}

// Save then Load round-trips every field: the watermark, the LSN through
// pg_lsn, the phase by name, and UpdatedAt from the store's clock rather
// than from the record handed in.
func TestSaveThenLoadRoundTripsTheRecord(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	cp := ordersCheckpoint()
	cp.UpdatedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), cp))

	got, err := f.store.Load(t.Context(), "app", "orders", cp.Fingerprints())
	require.NoError(t, err)
	assert.True(t, stampedAt.Equal(got.UpdatedAt), "UpdatedAt is the store clock's reading, got %s", got.UpdatedAt)
	got.UpdatedAt = time.Time{}
	cp.UpdatedAt = time.Time{}
	assert.Equal(t, cp, got)
}

// A second Save for the same target replaces the one row rather than adding
// another (ST-1): the table still holds one row and Load returns the later
// watermark, phase, and clock reading.
func TestSaveUpsertsTheOneRowPerTarget(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	lock := f.ordersLock(t)
	first := ordersCheckpoint()
	require.NoError(t, f.store.Save(t.Context(), lock, first))

	later := stampedAt.Add(time.Minute)
	f.clock.now = later
	second := first
	second.Watermark = copier.NewWatermark(5000)
	second.LastAppliedLSN = decode.LSN(0x00000016b374ffff)
	second.Phase = checkpoint.PhaseCatchingUp
	second.SlotName = "pgsprite_0a1b2c3d"
	second.PublicationName = "pgsprite_0a1b2c3d"
	second.ShadowTable = "_pgsprite_orders_new2"
	require.NoError(t, f.store.Save(t.Context(), lock, second))

	assert.Equal(t, int64(1), f.rowCount(t))
	got, err := f.store.Load(t.Context(), "app", "orders", first.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(5000), got.Watermark)
	assert.Equal(t, decode.LSN(0x00000016b374ffff), got.LastAppliedLSN)
	assert.Equal(t, checkpoint.PhaseCatchingUp, got.Phase)
	assert.Equal(t, "pgsprite_0a1b2c3d", got.SlotName)
	assert.Equal(t, "pgsprite_0a1b2c3d", got.PublicationName)
	assert.Equal(t, "_pgsprite_orders_new2", got.ShadowTable)
	assert.True(t, later.Equal(got.UpdatedAt))
}

// Rows for different targets live side by side; a save for one never
// touches the other.
func TestSaveKeepsOneRowPerTarget(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	orders := ordersCheckpoint()
	customers := ordersCheckpoint()
	customers.Table = "customers"
	customers.ShadowTable = "_pgsprite_customers_new"
	customers.Watermark = copier.Watermark{}
	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), orders))
	require.NoError(t, f.store.Save(t.Context(), f.lock(t, "app", "customers"), customers))
	assert.Equal(t, int64(2), f.rowCount(t))

	got, err := f.store.Load(t.Context(), "app", "customers", customers.Fingerprints())
	require.NoError(t, err)
	assert.False(t, got.Watermark.Valid(), "a run that has landed nothing reads back the zero watermark")
	assert.Equal(t, "_pgsprite_customers_new", got.ShadowTable)
}

// A Save for a target whose row carries another statement's fingerprints is
// refused with the typed mismatch and leaves the stored row exactly as it
// was (ST-2): the statement's guard updated nothing. Delete is the explicit
// fresh start, after which the new statement's Save lands.
func TestSaveRefusesToOverwriteAnotherStatementsRow(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	lock := f.ordersLock(t)
	original := ordersCheckpoint()
	require.NoError(t, f.store.Save(t.Context(), lock, original))

	other := ordersCheckpoint()
	other.TargetFingerprint = "tgt-other"
	other.Watermark = copier.NewWatermark(7)
	other.Phase = checkpoint.PhaseVerifying
	err := f.store.Save(t.Context(), lock, other)
	var incompatible *checkpoint.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, &checkpoint.IncompatibleError{
		Schema: "app", Table: "orders", Mismatch: checkpoint.MismatchTarget, Have: "tgt-a", Want: "tgt-other",
		Stored: original.Identity(),
	}, incompatible)

	kept, err := f.store.Load(t.Context(), "app", "orders", original.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(1000), kept.Watermark, "the refused save changed nothing")
	assert.Equal(t, checkpoint.PhaseCopying, kept.Phase)
	assert.Equal(t, int64(1), f.rowCount(t))

	require.NoError(t, f.store.Delete(t.Context(), lock, "app", "orders", incompatible.Stored))
	require.NoError(t, f.store.Save(t.Context(), lock, other))
	got, err := f.store.Load(t.Context(), "app", "orders", other.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(7), got.Watermark)
	assert.Equal(t, int64(1), f.rowCount(t))
}

// Delete removes only the row the caller was shown (ST-2): a row another
// statement wrote after the caller read its IncompatibleError is not the
// one it agreed to discard, so the row stays and the caller is shown the
// new one.
func TestDeleteRefusesARowTheCallerWasNotShown(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	lock := f.ordersLock(t)
	shown := ordersCheckpoint()
	require.NoError(t, f.store.Save(t.Context(), lock, shown))
	newer := ordersCheckpoint()
	newer.TargetFingerprint = "tgt-newer"
	newer.Watermark = copier.NewWatermark(42)
	require.NoError(t, f.store.Delete(t.Context(), lock, "app", "orders", shown.Identity()))
	require.NoError(t, f.store.Save(t.Context(), lock, newer))

	err := f.store.Delete(t.Context(), lock, "app", "orders", shown.Identity())
	var incompatible *checkpoint.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, &checkpoint.IncompatibleError{
		Schema: "app", Table: "orders", Mismatch: checkpoint.MismatchTarget, Have: "tgt-newer", Want: "tgt-a",
		Stored: newer.Identity(),
	}, incompatible)
	kept, err := f.store.Load(t.Context(), "app", "orders", newer.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(42), kept.Watermark, "the newer row is untouched")
}

// A checkpoint write without the target's lock session, or with a session
// for another table, is refused before any connection is opened (LK-1):
// the store never writes resume state for a table it does not hold.
func TestSaveAndDeleteRequireTheTargetsTableLock(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	cp := ordersCheckpoint()

	assert.ErrorIs(t, f.store.Save(t.Context(), nil, cp), checkpoint.ErrInvariantViolation)
	assert.ErrorIs(t, f.store.Delete(t.Context(), nil, "app", "orders", cp.Identity()), checkpoint.ErrInvariantViolation)

	customers := f.lock(t, "app", "customers")
	assert.ErrorIs(t, f.store.Save(t.Context(), customers, cp), checkpoint.ErrInvariantViolation)
	assert.ErrorIs(t, f.store.Delete(t.Context(), customers, "app", "orders", cp.Identity()), checkpoint.ErrInvariantViolation)
	assert.Equal(t, int64(0), f.rowCount(t))
}

// A Save whose lock session the server no longer counts as the holder is
// refused as the store's invariant violation (LK-1), and the newer holder's
// row is left as it was. The stale session's backend is terminated under a
// keepalive too long to notice during the test, so the session still
// believes it holds the table and only the confirmation from the save's own
// transaction stands between it and the row. This is the stale-writer case:
// same statement, same fingerprints, older state.
func TestSaveRefusesWhenAnotherBackendHoldsTheTable(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	stale, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: f.url}, "app", "orders", dbconn.WithTableLockKeepalive(time.Hour))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.Error(t, stale.Release(context.WithoutCancel(t.Context())), "releasing a dead session reports it")
	})
	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT pg_terminate_backend($1)", stale.BackendPID()).Scan(&terminated))
	require.True(t, terminated)
	const backendExitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var alive bool
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)", stale.BackendPID()).Scan(&alive))
		return !alive
	}, backendExitDeadline, 50*time.Millisecond, "the terminated backend should leave pg_stat_activity")
	require.NoError(t, stale.Err(), "the stale session has not noticed its loss")

	takeover := f.ordersLock(t)
	newer := ordersCheckpoint()
	newer.Watermark = copier.NewWatermark(1800)
	newer.Phase = checkpoint.PhaseVerifying
	require.NoError(t, f.store.Save(t.Context(), takeover, newer))

	err = f.store.Save(t.Context(), stale, ordersCheckpoint())
	assert.ErrorIs(t, err, checkpoint.ErrInvariantViolation)
	var heldElsewhere *dbconn.TableLockHeldError
	assert.ErrorAs(t, err, &heldElsewhere, "the server names the backend that holds the table now")
	got, err := f.store.Load(t.Context(), "app", "orders", newer.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(1800), got.Watermark, "the stale save changed nothing")
	assert.Equal(t, checkpoint.PhaseVerifying, got.Phase)
}

// Delete of a target with no row is not an error: the target is already in
// the state Delete produces.
func TestDeleteIsIdempotent(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	lock := f.ordersLock(t)
	cp := ordersCheckpoint()
	require.NoError(t, f.store.Delete(t.Context(), lock, "app", "orders", cp.Identity()))
	require.NoError(t, f.store.Save(t.Context(), lock, cp))
	require.NoError(t, f.store.Delete(t.Context(), lock, "app", "orders", cp.Identity()))
	require.NoError(t, f.store.Delete(t.Context(), lock, "app", "orders", cp.Identity()))
	_, err := f.store.Load(t.Context(), "app", "orders", cp.Fingerprints())
	assert.ErrorIs(t, err, checkpoint.ErrNotFound)
}

// An invalid checkpoint never reaches the database.
func TestSaveRefusesAnInvalidCheckpointBeforeTheDatabase(t *testing.T) {
	f := newStoreFixture(t)
	cp := ordersCheckpoint()
	cp.TargetFingerprint = ""
	err := f.store.Save(t.Context(), f.ordersLock(t), cp)
	assert.ErrorIs(t, err, checkpoint.ErrInvalidCheckpoint)
	var pgErr *pgconn.PgError
	assert.False(t, errors.As(err, &pgErr), "the database was never asked")
	assert.False(t, f.tableExists(t), "Save does not create the table; Ensure does")
}

// Without Ensure the table is missing, and Save, Delete, and Load all report
// the typed ErrTableMissing — still carrying the server's undefined_table —
// rather than creating anything on their own; Load's answer is never
// ErrNotFound, so resume cannot start fresh over an unprovisioned database.
func TestWritesAndReadsWithoutEnsureReportTheMissingTable(t *testing.T) {
	f := newStoreFixture(t)
	lock := f.ordersLock(t)
	cp := ordersCheckpoint()
	for name, err := range map[string]error{
		"save":   f.store.Save(t.Context(), lock, cp),
		"delete": f.store.Delete(t.Context(), lock, "app", "orders", cp.Identity()),
	} {
		assert.ErrorIs(t, err, checkpoint.ErrTableMissing, name)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, name)
		assert.Equal(t, "42P01", pgErr.Code, name)
	}
	_, err := f.store.Load(t.Context(), "app", "orders", cp.Fingerprints())
	assert.ErrorIs(t, err, checkpoint.ErrTableMissing)
	assert.NotErrorIs(t, err, checkpoint.ErrNotFound)
	assert.False(t, f.tableExists(t))
}

// A cancelled context stops Save before any write.
func TestSaveHonoursACancelledContext(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	lock := f.ordersLock(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := f.store.Save(ctx, lock, ordersCheckpoint())
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(0), f.rowCount(t))
}
