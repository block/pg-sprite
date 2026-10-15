package checksum_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
)

// A repair puts a converted chunk back the way the copy landed it: the
// recopy runs the copy statement with the statement's USING expression in
// the converted column's place, on the build's search_path, so the
// unqualified schema-local function the expression calls resolves as it
// did when the statement ran on the shadow. The function widens qty to
// bigint, which is what the digest's cast of the source does too, so the
// pass can see the tampered chunk differ and the repaired one agree.
func TestCheckRepairsAConvertedChunkThroughTheStatementsUsing(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	f.exec(t, `
		CREATE FUNCTION %s.widen(qty integer) RETURNS bigint
			LANGUAGE sql IMMUTABLE
			AS 'SELECT qty::bigint'`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			DROP COLUMN note,
			ALTER COLUMN qty TYPE bigint USING widen(qty)`)
	require.Equal(t, map[string]string{"qty": "widen(qty)"}, shadow.Conversions())
	f.copy(t, target, shadow, lock)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.NoError(t, err)
	require.Len(t, outcome.Repairs, 1)
	assert.Equal(t, chunk(t, 1001, 2000), outcome.Repairs[0].Mismatch.Chunk)
	assert.Equal(t, int64(1000), outcome.Repairs[0].Inserted, "the recopy converted and landed the whole chunk")
	qty, _ := f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(1500), qty, "the tampered row holds the converted source value again")

	again, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.NoError(t, err)
	assert.True(t, again.Clean(), "the fresh pass finds the shadow equal to the source")
}

// A repair recopies through the statement's USING expression, not through
// the server's assignment cast. PostgreSQL has no assignment cast from text
// to integer, so a recopy that selected the column bare would fail on its
// first chunk; the expression is the only thing that lands it. The digest's
// explicit cast of the source reproduces this expression, so the pass sees
// the tampered chunk differ and the repaired one agree.
func TestCheckRepairRecopiesThroughTheUsingNotTheAssignmentCast(t *testing.T) {
	f := newVerifierFixture(t)
	f.exec(t, `
		CREATE TABLE %s.ledger (
			id bigint PRIMARY KEY,
			amount text NOT NULL
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.ledger (id, amount)
		SELECT n, n::text FROM generate_series(1, %d) AS n`, rows))
	target := f.prove(t, "ledger")
	lock := f.lock(t, "ledger")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN amount TYPE integer USING amount::integer`)
	require.Equal(t, map[string]string{"amount": "amount::int"}, shadow.Conversions())
	f.copy(t, target, shadow, lock)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET amount = 0 WHERE id = 1500")

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.NoError(t, err)
	require.Len(t, outcome.Repairs, 1)
	assert.Equal(t, chunk(t, 1001, 2000), outcome.Repairs[0].Mismatch.Chunk)
	assert.Equal(t, int64(1000), outcome.Repairs[0].Inserted, "the recopy converted and landed the whole chunk")
	var amount int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT amount FROM "+f.shadowName(shadow)+" WHERE id = 1500").Scan(&amount))
	assert.Equal(t, int64(1500), amount, "the tampered row holds the converted source value again")
}
