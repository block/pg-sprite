package checksum_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
)

// A pass digests the source through the statement's USING expression, so
// an operator created on the build's search_path after the copy — here an
// integer * integer that answers zero, exactly the signature the
// expression spells — would make the source digest to something the
// shadow never held, and a repair would then write the zero into every
// chunk. The proof's conversion fingerprint pins the catalog the build
// saw; the pass recomputes it before its first chunk, finds the planted
// operator, and refuses as an invariant violation instead of comparing or
// repairing anything.
func TestCheckRefusesWhenAConversionResolvesDifferentlyThanAtTheBuild(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			DROP COLUMN note,
			ALTER COLUMN qty TYPE bigint USING (qty * 1)::bigint`)
	require.Equal(t, map[string]string{"qty": "(qty * 1)::bigint"}, shadow.Conversions())
	require.NotEmpty(t, shadow.ConversionFingerprint(), "a copy with a conversion carries a fingerprint")
	f.copy(t, target, shadow, lock)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")

	f.exec(t, `
		CREATE FUNCTION %s.times_zero(integer, integer) RETURNS integer
			LANGUAGE sql IMMUTABLE
			AS 'SELECT 0'`)
	f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = integer, RIGHTARG = integer, FUNCTION = %s.times_zero)`)

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	assert.Equal(t, checksum.Report{}, outcome.Report, "the pass compared nothing")
	assert.Empty(t, outcome.Repairs, "the pass repaired nothing")
	qty, exists := f.shadowRow(t, shadow, 1500)
	require.True(t, exists)
	assert.Equal(t, int64(0), qty, "the tampered row is untouched: no repair ran through the planted operator")
}
