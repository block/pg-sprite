package checkpoint_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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

func (f storeFixture) rowCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM pgsprite.pgsprite_checkpoint").Scan(&n))
	return n
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

// Several engines reaching the same database for the first time at once
// all succeed: the advisory lock serializes their creates, so none of them
// sees the duplicate-key failure concurrent CREATE … IF NOT EXISTS can
// raise in the catalog.
func TestEnsureSerializesConcurrentFirstUse(t *testing.T) {
	f := newStoreFixture(t)
	const engines = 8
	errs := make([]error, engines)
	var wg sync.WaitGroup
	for i := range engines {
		wg.Go(func() { errs[i] = f.store.Ensure(t.Context()) })
	}
	wg.Wait()
	for i, err := range errs {
		assert.NoError(t, err, "engine %d", i)
	}
	assert.True(t, f.tableExists(t))
}

// Save then Load round-trips every field: the watermark, the LSN through
// pg_lsn, the phase by name, and UpdatedAt from the store's clock rather
// than from the record handed in.
func TestSaveThenLoadRoundTripsTheRecord(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	cp := ordersCheckpoint()
	cp.UpdatedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, f.store.Save(t.Context(), cp))

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
	first := ordersCheckpoint()
	require.NoError(t, f.store.Save(t.Context(), first))

	later := stampedAt.Add(time.Minute)
	f.clock.now = later
	second := first
	second.Watermark = copier.NewWatermark(5000)
	second.LastAppliedLSN = decode.LSN(0x00000016b374ffff)
	second.Phase = checkpoint.PhaseCatchingUp
	second.SlotName = "pgsprite_0a1b2c3d"
	second.PublicationName = "pgsprite_0a1b2c3d"
	require.NoError(t, f.store.Save(t.Context(), second))

	assert.Equal(t, int64(1), f.rowCount(t))
	got, err := f.store.Load(t.Context(), "app", "orders", first.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(5000), got.Watermark)
	assert.Equal(t, decode.LSN(0x00000016b374ffff), got.LastAppliedLSN)
	assert.Equal(t, checkpoint.PhaseCatchingUp, got.Phase)
	assert.Equal(t, "pgsprite_0a1b2c3d", got.SlotName)
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
	require.NoError(t, f.store.Save(t.Context(), orders))
	require.NoError(t, f.store.Save(t.Context(), customers))
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
	original := ordersCheckpoint()
	require.NoError(t, f.store.Save(t.Context(), original))

	other := ordersCheckpoint()
	other.TargetFingerprint = "tgt-other"
	other.Watermark = copier.NewWatermark(7)
	other.Phase = checkpoint.PhaseVerifying
	err := f.store.Save(t.Context(), other)
	var incompatible *checkpoint.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, &checkpoint.IncompatibleError{
		Schema: "app", Table: "orders", Mismatch: checkpoint.MismatchTarget, Have: "tgt-a", Want: "tgt-other",
	}, incompatible)

	kept, err := f.store.Load(t.Context(), "app", "orders", original.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(1000), kept.Watermark, "the refused save changed nothing")
	assert.Equal(t, checkpoint.PhaseCopying, kept.Phase)
	assert.Equal(t, int64(1), f.rowCount(t))

	require.NoError(t, f.store.Delete(t.Context(), "app", "orders"))
	require.NoError(t, f.store.Save(t.Context(), other))
	got, err := f.store.Load(t.Context(), "app", "orders", other.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(7), got.Watermark)
	assert.Equal(t, int64(1), f.rowCount(t))
}

// Delete of a target with no row is not an error: the target is already in
// the state Delete produces.
func TestDeleteIsIdempotent(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	require.NoError(t, f.store.Delete(t.Context(), "app", "orders"))
	require.NoError(t, f.store.Save(t.Context(), ordersCheckpoint()))
	require.NoError(t, f.store.Delete(t.Context(), "app", "orders"))
	require.NoError(t, f.store.Delete(t.Context(), "app", "orders"))
	_, err := f.store.Load(t.Context(), "app", "orders", ordersCheckpoint().Fingerprints())
	assert.ErrorIs(t, err, checkpoint.ErrNotFound)
}

// An invalid checkpoint never reaches the database.
func TestSaveRefusesAnInvalidCheckpointBeforeTheDatabase(t *testing.T) {
	f := newStoreFixture(t)
	cp := ordersCheckpoint()
	cp.TargetFingerprint = ""
	err := f.store.Save(t.Context(), cp)
	assert.ErrorIs(t, err, checkpoint.ErrInvalidCheckpoint)
	var pgErr *pgconn.PgError
	assert.False(t, errors.As(err, &pgErr), "the database was never asked")
	assert.False(t, f.tableExists(t), "Save does not create the table; Ensure does")
}

// Without Ensure the table is missing, and Save reports the server's
// undefined_table rather than creating anything on its own.
func TestSaveWithoutEnsureReportsTheMissingTable(t *testing.T) {
	f := newStoreFixture(t)
	err := f.store.Save(t.Context(), ordersCheckpoint())
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42P01", pgErr.Code)
}

// A cancelled context stops Save before any write.
func TestSaveHonoursACancelledContext(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := f.store.Save(ctx, ordersCheckpoint())
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(0), f.rowCount(t))
}
