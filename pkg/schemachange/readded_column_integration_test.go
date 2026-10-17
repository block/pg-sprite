package schemachange_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
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

// The same drop and re-add on a live run: the stream and the swap's drain
// decode the dropped column's values on every change they carry, including
// an update that touches only that column and an insert drained under the
// swap's lock. None of them reaches the new column, which holds its default
// on every live row. The old-versus-live convergence check is left out on
// purpose: the two tables differ in the re-added column by design.
func TestRunnerLiveRunGivesAReaddedColumnItsDefault(t *testing.T) {
	const seeded = 2500
	f := newDecodingFixture(t, seeded)
	w := f.startWriter(t, seeded, liveWriterInterval)

	drained := func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCutover {
			return nil
		}
		for _, sql := range []string{
			`UPDATE %s SET label = 'label-only' WHERE id = 3`,
			`INSERT INTO %s (uniq, amount, label, blob) VALUES (900001, 9, 'drained', 'x')`,
			`UPDATE %s SET amount = 777, label = 'both' WHERE id = 1`,
		} {
			if _, err := f.pool.Exec(ctx, fmt.Sprintf(sql, f.table.Qualified())); err != nil {
				return err
			}
		}
		return nil
	}
	st, err := statement.ParseOne(fmt.Sprintf(`
		ALTER TABLE %s
			DROP COLUMN label,
			ADD COLUMN label text DEFAULT 'fresh',
			ALTER COLUMN amount TYPE numeric(14,2)`, f.table.Qualified()))
	require.NoError(t, err)
	var writes int
	r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{
		AfterCheckpoint: untilVerifying(t, w, &writes, drained)}))
	require.NoError(t, err)

	result, err := r.Run(t.Context(), f.pool, f.target, st)
	require.NoError(t, err)

	assert.True(t, result.Verification.Clean())
	assert.Positive(t, writes, "the source was written while the run was in progress")
	assert.NotContains(t, result.Shadow.CopyColumns, "label", "the re-added column is not a copy column")
	var notFresh, drainedRows int
	var amount string
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`
		SELECT count(*) FILTER (WHERE label IS DISTINCT FROM 'fresh'),
		       count(*) FILTER (WHERE uniq = 900001),
		       max(amount::text) FILTER (WHERE id = 1)
		FROM %s`, f.table.Qualified())).Scan(&notFresh, &drainedRows, &amount))
	assert.Zero(t, notFresh, "every live row, including the streamed and drained ones, holds the new column's default")
	assert.Equal(t, 1, drainedRows, "the drained insert is live")
	assert.Equal(t, "777.00", amount, "the drained update's retyped column was carried")
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
