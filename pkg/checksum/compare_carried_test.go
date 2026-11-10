package checksum

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// A carried key differs when the two sides hold different rows for it or
// when only one side holds a row at all. A key neither side holds is the
// same on both: the source deleted the row and the stream carried the
// delete. The result keeps the order the keys were given in.
func TestDifferingKeysJudgesEachCarriedKeyOnBothSides(t *testing.T) {
	a, b := hashOf('a'), hashOf('b')
	source := rowSet{{1, a}, {2, a}, {4, a}}
	shadow := rowSet{{1, a}, {2, b}, {3, a}}

	differing := differingKeys([]int64{1, 2, 3, 4, 5}, source, shadow)

	assert.Equal(t, []int64{2, 3, 4}, differing,
		"2 holds another row on the shadow, 3 is missing from the source, 4 is missing from the shadow; 1 is equal and 5 is absent from both")
	assert.Nil(t, differingKeys([]int64{1, 5}, source, shadow), "every key equal names none")
}

// A verified-shadow proof is compared only against the shadow it was
// minted over: the same schema, source, and shadow names and the same two
// relations. A proof that differs in any of them names keys of another
// pair of tables (ST-6).
func TestCheckVerifiedOverRefusesAProofOfAnotherShadow(t *testing.T) {
	shadow := fakeShadow{schema: "app", source: "orders", shadow: "_pgsprite_orders_new", sourceOID: 1, shadowOID: 2, columns: []string{"id"}}
	mint := func(s fakeShadow) VerifiedShadow {
		return newVerifiedShadow(s, copier.NewWatermark(math.MaxInt64), time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC), true)
	}
	require.NoError(t, checkVerifiedOver(shadow, mint(shadow)))

	tests := map[string]struct {
		verified VerifiedShadow
		want     string
	}{
		"zero proof":     {VerifiedShadow{}, "verified shadow proof is empty"},
		"another schema": {mint(withSchema(shadow, "other")), "verified shadow proof is for other.orders into _pgsprite_orders_new, shadow proof is for app.orders into _pgsprite_orders_new"},
		"another source": {mint(withSource(shadow, "invoices")), "verified shadow proof is for app.invoices into _pgsprite_orders_new, shadow proof is for app.orders into _pgsprite_orders_new"},
		"another shadow": {mint(withShadowTable(shadow, "_pgsprite_invoices_new")), "verified shadow proof is for app.orders into _pgsprite_invoices_new, shadow proof is for app.orders into _pgsprite_orders_new"},
		"rebuilt source": {mint(withOIDs(shadow, 7, 2)), "verified shadow proof compared relations 7 and 2, shadow proof is for relations 1 and 2"},
		"rebuilt shadow": {mint(withOIDs(shadow, 1, 8)), "verified shadow proof compared relations 1 and 8, shadow proof is for relations 1 and 2"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := checkVerifiedOver(shadow, tt.verified)
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.ErrorContains(t, err, "(ST-6)")
			assert.ErrorContains(t, err, tt.want)
		})
	}
}
