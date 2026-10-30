package applier_test

import (
	"context"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
)

// A flush writes a retyped column through the statement's USING expression
// on the build's search_path, so an operator created on that path after
// the build — here a numeric * integer that answers zero, exactly the
// signature the expression spells — would silently land zero cents for
// every image from then on. The proof's conversion fingerprint pins the
// catalog the build saw; the flush recomputes it before writing, finds the
// planted operator, and refuses as an invariant violation with the shadow
// untouched.
func TestFlushRefusesWhenAConversionResolvesDifferentlyThanAtTheBuild(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 50)
	p := f.preparePriced(t, priceInCents)
	require.NotEmpty(t, p.shadow.ConversionFingerprint(), "a copy with a conversion carries a fingerprint")
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)

	f.exec(t, `
		CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
			LANGUAGE sql IMMUTABLE
			AS 'SELECT 0'`)
	f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.times_zero)`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
	))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Equal(t, applier.Result{}, result, "nothing was written")
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "525", *price5, "the shadow still holds the copied value, not zero cents from the planted operator")
}

// The flush's guard confirms the fingerprint before its writes, and the
// writes resolve the expression in statements of their own, so an operator
// committed between the two is resolved by the write: here the operator is
// planted just as the image's INSERT is about to be sent, after the guard
// passed, and the INSERT lands zero cents. The flush confirms the
// fingerprint once more after its writes and before its commit; under READ
// COMMITTED that read sees the planted operator, so the flush refuses and
// the batch rolls back.
func TestFlushRollsBackABatchWhoseWriteRanAfterTheConversionDrifted(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 50)
	p := f.preparePriced(t, priceInCents)
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	pool := f.poolPlantingTimesZeroBeforeFirstInsert(t, nil)

	result, err := p.flush.Flush(t.Context(), pool, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
	))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Equal(t, applier.Result{}, result, "nothing was written")
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "525", *price5, "the write that ran through the planted operator rolled back")
}

// The same drift under a pool whose sessions default to REPEATABLE READ:
// there the repeated read would see the flush's first snapshot, taken
// before the operator was planted, and the batch would commit zero cents.
// The flush transaction pins READ COMMITTED whatever the session default,
// so the repeated read still sees the operator and the batch rolls back.
func TestFlushRollsBackADriftedBatchWhenSessionsDefaultToRepeatableRead(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 50)
	p := f.preparePriced(t, priceInCents)
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	pool := f.poolPlantingTimesZeroBeforeFirstInsert(t, map[string]string{"default_transaction_isolation": "repeatable read"})
	var isolation string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_setting('transaction_isolation')`).Scan(&isolation))
	require.Equal(t, "repeatable read", isolation, "the pool's sessions default to REPEATABLE READ")

	result, err := p.flush.Flush(t.Context(), pool, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
	))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Equal(t, applier.Result{}, result, "nothing was written")
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "525", *price5, "the write that ran through the planted operator rolled back")
}

// poolPlantingTimesZeroBeforeFirstInsert is a pool on the fixture's
// database, with params laid over its sessions' settings, whose
// connections plant a `numeric * integer` that answers zero on the
// build's path — once, just before the first INSERT any of them sends.
func (f flushFixture) poolPlantingTimesZeroBeforeFirstInsert(t *testing.T, params map[string]string) *pgxpool.Pool {
	t.Helper()
	plant := &beforeFirstInsert{hook: func() {
		f.exec(t, `
			CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
				LANGUAGE sql IMMUTABLE
				AS 'SELECT 0'`)
		f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.times_zero)`)
	}}
	poolConfig, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	poolConfig.ConnConfig.Tracer = plant
	maps.Copy(poolConfig.ConnConfig.RuntimeParams, params)
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// beforeFirstInsert is a pgx query tracer that runs hook once, before the
// first INSERT its connections send. It is a test-only tracer: production
// code connects through dbconn.
type beforeFirstInsert struct {
	hook func()
	once sync.Once
}

func (b *beforeFirstInsert) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "INSERT INTO") {
		b.once.Do(b.hook)
	}
	return ctx
}

func (b *beforeFirstInsert) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
