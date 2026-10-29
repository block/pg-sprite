package applier_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
)

// beginCallerTx opens the read-write transaction a FlushIn caller owns —
// the cutover's swap transaction in production — on pool, rolled back at
// cleanup if the test has not ended it.
func beginCallerTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	require.NoError(t, err)
	t.Cleanup(func() {
		// The test has usually committed or rolled back already; a closed
		// transaction reports pgx.ErrTxClosed, which says nothing here.
		_ = tx.Rollback(context.WithoutCancel(t.Context()))
	})
	return tx
}

// The drain the cutover runs under its swap lock writes a retyped column
// through the statement's USING expression, which resolves its unqualified
// names on the build's search_path — the source schema then public, with
// the catalog ahead of both (CO-9). The expression here calls a function
// that lives only in the source schema: on the catalog alone it does not
// resolve, and the drain would fail the swap. The drained images land
// with the values the function converts them to, committed with the
// caller's transaction.
func TestFlushInResolvesConversionNamesOnTheBuildsSearchPath(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 20)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(amount text) RETURNS integer
			LANGUAGE sql IMMUTABLE
			AS 'SELECT (coalesce(amount, ''0'')::numeric * 100)::integer'`)
	p := f.preparePriced(t, `to_cents(price)`)
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	f.exec(t, `INSERT INTO %s.orders (id, price, note) VALUES (21, '0.99', 'order 21')`)
	tx := beginCallerTx(t, f.pool)

	result, err := p.flush.FlushIn(t.Context(), tx, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
		image(21, col("price", "0.99"), col("note", "order 21")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2}, result)
	require.NoError(t, tx.Commit(t.Context()))
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "750", *price5, "the schema's function converted the value")
	price21, _ := f.text(t, p.shadow.ShadowTable(), "price", 21)
	assert.Equal(t, "99", *price21)
	f.assertPricesInCents(t, p.shadow)
}

// An unqualified operator can resolve to one thing on the build's path and
// to another on the catalog alone: here the source schema holds an
// exact-type numeric * integer that answers zero, so the gated statement —
// and the copy — converted every price to zero cents, while the catalog's
// numeric * numeric would convert 7.50 to 750. The drain evaluates the
// expression on the build's path, so the image it writes under the swap
// lock holds what the statement produced for every other row.
func TestFlushInConvertsAsTheBuildDidWhenThePathChangesTheOperator(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 20)
	f.exec(t, `
		CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
			LANGUAGE sql IMMUTABLE
			AS 'SELECT 0'`)
	f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.times_zero)`)
	p := f.preparePriced(t, priceInCents)
	price4, _ := f.text(t, p.shadow.ShadowTable(), "price", 4)
	require.Equal(t, "0", *price4, "the copy converted through the schema's operator")
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	tx := beginCallerTx(t, f.pool)

	result, err := p.flush.FlushIn(t.Context(), tx, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 1}, result)
	require.NoError(t, tx.Commit(t.Context()))
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "0", *price5, "the drained image converted through the operator the build resolved, not the catalog's")
}

// An operator created on the build's path after the build would make the
// drain land zero cents under the swap lock, committed with the renames.
// The drain recomputes the proof's conversion fingerprint before its
// writes, finds the planted operator, and refuses as an invariant
// violation; the caller rolls back and the shadow is untouched.
func TestFlushInRefusesWhenAConversionResolvesDifferentlyThanAtTheBuild(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 50)
	p := f.preparePriced(t, priceInCents)
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	f.exec(t, `
		CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
			LANGUAGE sql IMMUTABLE
			AS 'SELECT 0'`)
	f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.times_zero)`)
	tx := beginCallerTx(t, f.pool)

	result, err := p.flush.FlushIn(t.Context(), tx, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
	))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Equal(t, applier.Result{}, result, "nothing was written")
	require.NoError(t, tx.Rollback(t.Context()))
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "525", *price5, "the shadow still holds the copied value, not zero cents from the planted operator")
}

// The drain confirms the fingerprint before it writes anything: a function
// the conversion calls, dropped after the build, is refused as the
// invariant violation it is, before the write that calls it could fail on
// an undefined function.
func TestFlushInRefusesBeforeWritingWhenAConversionsFunctionIsDropped(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 20)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(amount text) RETURNS integer
			LANGUAGE sql IMMUTABLE
			AS 'SELECT (coalesce(amount, ''0'')::numeric * 100)::integer'`)
	p := f.preparePriced(t, `to_cents(price)`)
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	f.exec(t, `DROP FUNCTION %s.to_cents(text)`)
	tx := beginCallerTx(t, f.pool)

	result, err := p.flush.FlushIn(t.Context(), tx, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
	))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Equal(t, applier.Result{}, result, "nothing was written")
	require.NoError(t, tx.Rollback(t.Context()))
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "525", *price5, "the shadow still holds the copied value")
}

// The drain confirms the fingerprint before its writes and once more after
// them: an operator committed just as the image's INSERT is sent is
// resolved by that INSERT, and the second read, under READ COMMITTED, sees
// it. The drain refuses, so the caller rolls the swap back instead of
// committing a value the gated statement would not have produced.
func TestFlushInRefusesABatchWhoseWriteRanAfterTheConversionDrifted(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 50)
	p := f.preparePriced(t, priceInCents)
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	tx := beginCallerTx(t, f.poolPlantingTimesZeroBeforeFirstInsert(t, nil))

	result, err := p.flush.FlushIn(t.Context(), tx, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
	))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Equal(t, applier.Result{}, result, "nothing was written")
	require.NoError(t, tx.Rollback(t.Context()))
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "525", *price5, "the write that ran through the planted operator rolled back")
}
