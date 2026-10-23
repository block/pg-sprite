package applier_test

import (
	"testing"

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
