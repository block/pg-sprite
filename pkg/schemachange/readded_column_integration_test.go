package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// One ALTER TABLE drops "note" and adds a new column of the same name. The
// new column is a different column that happens to reuse the name, so its
// value on every row is its own default, and the dropped column's values
// are not carried into it. The retype in the same statement still copies.
func TestRunnerGivesAReaddedColumnItsDefaultNotTheDroppedValues(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)

	result, err := f.run(t, "orders", `
		ALTER TABLE %s.orders
			DROP COLUMN note,
			ADD COLUMN note text DEFAULT 'fresh',
			ALTER COLUMN qty TYPE bigint`, schemachange.RunOptions{})
	require.NoError(t, err)

	assert.True(t, result.Verification.Clean())
	assert.Equal(t, []string{"id", "qty", "sku"}, result.Shadow.CopyColumns, "the re-added column is not a copy column")
	assert.Equal(t, "bigint", f.columnType(t, "orders", "qty"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))

	var fresh, oldValues, qtySum int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE note = 'fresh'),
		       count(*) FILTER (WHERE note LIKE 'note %'),
		       sum(qty)
		FROM `+f.schema+`.orders`).Scan(&fresh, &oldValues, &qtySum))
	assert.EqualValues(t, 2500, fresh, "every row holds the new column's default")
	assert.EqualValues(t, 0, oldValues, "no value of the dropped column survives")
	assert.EqualValues(t, 3126250, qtySum, "the retyped column kept its values")
}

// A re-added column with no default holds NULL on every row, and the copy
// list is the same whether the shadow was just built or re-derived from the
// catalog by a resume. The source already had a dropped column of its own,
// so its attribute numbers have a gap the shadow's do not: the columns that
// stayed are still recognised as the same columns, the one re-added is not.
func TestBuildShadowLeavesAReaddedColumnOutOfTheCopyWhenTheSourceHasADroppedColumn(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.items (
			id     bigint PRIMARY KEY,
			legacy text,
			label  text,
			qty    integer NOT NULL
		)`)
	f.exec(t, `ALTER TABLE %s.items DROP COLUMN legacy`)
	lock := f.lock(t, "items")
	target := f.prove(t, "items")
	alter := f.alter(t, `
		ALTER TABLE %s.items
			DROP COLUMN label,
			ADD COLUMN label text,
			ALTER COLUMN qty TYPE bigint`)

	built, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	require.NoError(t, err)
	assert.Equal(t, []string{"id", "qty"}, built.CopyColumns())

	inspected, err := schemachange.InspectShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	require.NoError(t, err)
	assert.Equal(t, built, inspected, "a resume re-derives the same copy columns")
}

// A change that drops a column and keeps the ones after it copies them all:
// the kept columns sit at the same positions on the shadow as on the
// source, whatever was dropped before or between them.
func TestBuildShadowCopiesEveryColumnTheChangeKeeps(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.items (
			id     bigint PRIMARY KEY,
			legacy text,
			label  text,
			qty    integer NOT NULL,
			note   text
		)`)
	f.exec(t, `ALTER TABLE %s.items DROP COLUMN legacy`)

	built, err := f.build(t, "items", `
		ALTER TABLE %s.items
			DROP COLUMN label,
			ALTER COLUMN qty TYPE bigint`)
	require.NoError(t, err)

	assert.Equal(t, []string{"id", "qty", "note"}, built.CopyColumns())
}
