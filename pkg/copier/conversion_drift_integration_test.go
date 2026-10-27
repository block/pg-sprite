package copier_test

import (
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// A converted copy resolves the USING expression's unqualified names on
// the build's search_path in every chunk, so an operator created on that
// path after the build — here a numeric * integer that answers zero,
// exactly the signature the expression spells, which the server would
// pick over the catalog's numeric * numeric — would silently change what
// every later chunk writes. The proof's conversion fingerprint pins the
// catalog the build saw; the first chunk recomputes it, finds the planted
// operator, and refuses as copy-conversion-drift before anything lands.
func TestCopierRefusesWhenAConversionResolvesDifferentlyThanAtTheBuild(t *testing.T) {
	f := newCopierFixture(t)
	f.createPricedOrders(t, 300)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING (price::numeric * 100)::integer`)
	require.NotEmpty(t, shadow.ConversionFingerprint(), "a copy with a conversion carries a fingerprint")

	f.exec(t, `
		CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
			LANGUAGE sql IMMUTABLE
			AS 'SELECT 0'`)
	f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.times_zero)`)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.ErrorIs(t, err, copier.ErrInvariantViolation)
	assert.Equal(t, copier.CauseConversionDrift, copier.RefusalCauseOf(err))
	assert.Zero(t, f.count(t, shadow.ShadowTable(), "true"), "no chunk landed through the planted operator")
	assert.Zero(t, c.Position().RowsInserted)
}

// A function the USING expression calls, replaced after the build with a
// different SQL-standard body, is a different conversion for every later
// chunk although its prosrc stays empty, so the copy refuses before any
// chunk lands.
func TestCopierRefusesAConversionFunctionWhoseSQLStandardBodyIsReplaced(t *testing.T) {
	f := newCopierFixture(t)
	f.createPricedOrders(t, 300)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(text) RETURNS integer
			LANGUAGE sql IMMUTABLE
			BEGIN ATOMIC SELECT ($1::numeric * 100)::integer; END`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING to_cents(price)`)

	f.exec(t, `
		CREATE OR REPLACE FUNCTION %s.to_cents(text) RETURNS integer
			LANGUAGE sql IMMUTABLE
			BEGIN ATOMIC SELECT ($1::numeric * 1000)::integer; END`)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.ErrorIs(t, err, copier.ErrInvariantViolation)
	assert.Equal(t, copier.CauseConversionDrift, copier.RefusalCauseOf(err))
	assert.Zero(t, f.count(t, shadow.ShadowTable(), "true"), "no chunk landed through the replaced body")
}

// Flipping a conversion function to STRICT after the build turns every NULL
// input into a NULL output instead of the function's answer for it — here
// 'none' for the half of the notes that are NULL — so the copy refuses
// before any chunk lands.
func TestCopierRefusesAConversionFunctionFlippedToStrict(t *testing.T) {
	f := newCopierFixture(t)
	f.createPricedOrders(t, 300)
	f.exec(t, `UPDATE %s.orders SET note = NULL WHERE id % 2 = 0`)
	f.exec(t, `
		CREATE FUNCTION %s.tag(text) RETURNS text
			LANGUAGE sql IMMUTABLE
			AS 'SELECT COALESCE($1, ''none'')'`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN note TYPE varchar(60) USING tag(note)`)

	f.exec(t, `ALTER FUNCTION %s.tag(text) STRICT`)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.ErrorIs(t, err, copier.ErrInvariantViolation)
	assert.Equal(t, copier.CauseConversionDrift, copier.RefusalCauseOf(err))
	assert.Zero(t, f.count(t, shadow.ShadowTable(), "true"), "no chunk landed through the strict function")
}

// The server rewrites `x NOT BETWEEN a AND b` into `x < a OR x > b`, so the
// conversion below resolves `<` and `>` on the build's path, never the
// `>=` and `<=` of a positive BETWEEN. A `numeric < integer` planted in the
// source schema after the build is an exact match the server prefers over
// the catalog's `numeric < numeric`, and it answers true for every row:
// every later chunk would land 1 where the build's resolution lands 0 for
// prices between 1 and 10. The copy refuses before any chunk lands.
func TestCopierRefusesWhenANotBetweenComparisonIsPlantedOnThePath(t *testing.T) {
	f := newCopierFixture(t)
	f.createPricedOrders(t, 300)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING ((price::numeric) NOT BETWEEN 1 AND 10)::integer`)

	f.exec(t, `
		CREATE FUNCTION %s.always_less(numeric, integer) RETURNS boolean
			LANGUAGE sql IMMUTABLE
			AS 'SELECT true'`)
	f.exec(t, `CREATE OPERATOR %s.< (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.always_less)`)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.ErrorIs(t, err, copier.ErrInvariantViolation)
	assert.Equal(t, copier.CauseConversionDrift, copier.RefusalCauseOf(err))
	assert.Zero(t, f.count(t, shadow.ShadowTable(), "true"), "no chunk landed through the planted operator")
}

// The guard's resolution check and the chunk's insert are separate
// statements, so an operator committed between them is resolved by the
// insert: here the first insert is parked after its guard passed, a
// `numeric * integer` that answers zero is planted, and the insert then
// lands zero cents for its whole chunk. The chunk confirms the fingerprint
// once more after its insert and before its commit; under READ COMMITTED
// that read sees the planted operator, so the chunk rolls back and nothing
// lands.
func TestCopierRollsBackAChunkWhoseInsertRanAfterTheConversionDrifted(t *testing.T) {
	f := newCopierFixture(t)
	parked := newInsertPause()
	pool := f.pausedPool(t, parked)

	shadowRows, inserted, err := f.copyWithADriftPlantedAtTheFirstInsert(t, pool, parked)

	require.ErrorIs(t, err, copier.ErrInvariantViolation)
	assert.Equal(t, copier.CauseConversionDrift, copier.RefusalCauseOf(err))
	assert.Zero(t, shadowRows, "the chunk whose insert ran through the planted operator rolled back")
	assert.Zero(t, inserted)
}

// The same drift under a pool whose sessions default to REPEATABLE READ:
// there the repeated read would see the transaction's first snapshot,
// taken before the operator was planted, and the chunk would commit zero
// cents. The chunk transaction pins READ COMMITTED whatever the session
// default, so the repeated read still sees the operator and the chunk
// rolls back.
func TestCopierRollsBackADriftedChunkWhenSessionsDefaultToRepeatableRead(t *testing.T) {
	f := newCopierFixture(t)
	parked := newInsertPause()
	poolConfig, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	poolConfig.ConnConfig.Tracer = parked
	poolConfig.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var isolation string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_setting('transaction_isolation')`).Scan(&isolation))
	require.Equal(t, "repeatable read", isolation, "the pool's sessions default to REPEATABLE READ")

	shadowRows, inserted, err := f.copyWithADriftPlantedAtTheFirstInsert(t, pool, parked)

	require.ErrorIs(t, err, copier.ErrInvariantViolation)
	assert.Equal(t, copier.CauseConversionDrift, copier.RefusalCauseOf(err))
	assert.Zero(t, shadowRows, "the chunk whose insert ran through the planted operator rolled back")
	assert.Zero(t, inserted)
}

// copyWithADriftPlantedAtTheFirstInsert copies a priced orders table whose
// shadow converts price to cents through `*`, on pool, whose connections
// park at their first insert on parked: while the first chunk's insert is
// parked after its guard passed, a `numeric * integer` that answers zero
// is planted on the build's path, and the insert then resumes. It returns
// the run's error, the rows the shadow holds afterwards, and the rows the
// copier counts as inserted.
func (f copierFixture) copyWithADriftPlantedAtTheFirstInsert(t *testing.T, pool *pgxpool.Pool, parked *statementPause) (int64, int64, error) {
	t.Helper()
	f.createPricedOrders(t, 100)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING (price::numeric * 100)::integer`)
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 1,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	result := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { result <- c.Run(t.Context(), pool) })
	t.Cleanup(wg.Wait)
	t.Cleanup(parked.resume)

	const parkDeadline = 15 * time.Second
	select {
	case <-parked.parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the copy did not reach its first chunk insert within %s", parkDeadline)
	}
	f.exec(t, `
		CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
			LANGUAGE sql IMMUTABLE
			AS 'SELECT 0'`)
	f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.times_zero)`)
	parked.resume()

	const stopDeadline = 30 * time.Second
	err = awaitRun(t, result, stopDeadline, "the copy")
	return f.count(t, shadow.ShadowTable(), "true"), c.Position().RowsInserted, err
}

// A copy that converts nothing resolves no expression, so its proof
// carries no fingerprint and nothing planted on the path can make it
// refuse: the catalog-only search_path a plain copy runs under never
// sees the schema's objects.
func TestCopierWithoutConversionsCarriesNoFingerprint(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	f.createPricedOrders(t, rows)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN note TYPE varchar(40)`)
	require.Empty(t, shadow.Conversions())
	assert.Empty(t, shadow.ConversionFingerprint(), "nothing to pin")

	f.exec(t, `
		CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
			LANGUAGE sql IMMUTABLE
			AS 'SELECT 0'`)
	f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.times_zero)`)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))
	assert.Equal(t, int64(rows), c.Position().RowsInserted)
}
