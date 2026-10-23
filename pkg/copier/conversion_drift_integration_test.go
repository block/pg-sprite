package copier_test

import (
	"testing"

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
