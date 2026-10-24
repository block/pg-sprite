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

// A repair's guard confirms the fingerprint before the recopy, and the
// recopy resolves the expression in statements of its own, so an operator
// committed between the two is resolved by the recopy: here a numeric *
// integer that answers zero — an exact match for the expression's
// `qty::numeric * 1`, which the server prefers over the catalog's numeric *
// numeric — is planted just as the recopy's INSERT is about to be sent,
// after the guard passed, and the INSERT lands zero for every row of the
// chunk. The repair confirms the fingerprint once more after the recopy
// and before its commit; under READ COMMITTED that read sees the planted
// operator, so the repair refuses and rolls back with the chunk as it was.
func TestCheckRollsBackARepairWhoseRecopyRanAfterTheConversionDrifted(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN qty TYPE bigint USING (qty::numeric * 1)::bigint`)
	f.copy(t, target, shadow, lock)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")

	pool := f.poolHookedBeforeStatement(t, "INSERT INTO", func() {
		f.exec(t, `
			CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
				LANGUAGE sql IMMUTABLE
				AS 'SELECT 0'`)
		f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = numeric, RIGHTARG = integer, FUNCTION = %s.times_zero)`)
	})

	outcome, err := f.check(t, pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	assert.Empty(t, outcome.Repairs, "no repair committed")
	qty, exists := f.shadowRow(t, shadow, 1499)
	require.True(t, exists, "the recopy's delete of the chunk rolled back")
	assert.Equal(t, int64(1499), qty, "the recopy that ran through the planted operator rolled back")
	qty, _ = f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(0), qty, "the tampered row is as it was before the repair")
}
