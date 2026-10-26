package schemachange_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// A live run whose USING expression calls a function defined only in the
// table's schema carries that conversion through the swap's drain: the
// drain flushes under the build's search_path, the schema then public
// with the catalog ahead of both (CO-9), as the copy and the catch-up do.
// On the catalog alone the function does not resolve and the swap would
// fail on the drained update. The update made after capture stopped —
// which only the drain carries — lands converted, and every live row holds
// its retained source amount in cents.
func TestRunnerLiveRunDrainsAConversionThatCallsAFunctionInTheTablesSchema(t *testing.T) {
	const seeded = 2500
	f := newDecodingFixture(t, seeded)
	_, err := f.pool.Exec(t.Context(), `
		CREATE FUNCTION `+pgx.Identifier{f.table.Schema}.Sanitize()+`.in_cents(amount numeric) RETURNS numeric
			LANGUAGE sql IMMUTABLE
			AS 'SELECT amount * 100'`)
	require.NoError(t, err)
	st, err := statement.ParseOne(fmt.Sprintf(`
		ALTER TABLE %s
			ALTER COLUMN amount TYPE numeric(14,2) USING in_cents(amount)`, f.table.Qualified()))
	require.NoError(t, err)
	drained := func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCutover {
			return nil
		}
		_, err := f.pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET amount = 7.77 WHERE id = 1`, f.table.Qualified()))
		return err
	}
	r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{AfterCheckpoint: drained}))
	require.NoError(t, err)

	result, err := r.Run(t.Context(), f.pool, f.target, st)

	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	var amount string
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT amount::text FROM %s WHERE id = 1`, f.table.Qualified())).Scan(&amount))
	assert.Equal(t, "777.00", amount, "the update drained under the swap's lock was converted by the schema's function")
	old := pgx.Identifier{f.table.Schema, schemachange.OldName(f.table.Schema, f.table.Table)}.Sanitize()
	var mismatched int
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`
		SELECT count(*)
		FROM %s o
		FULL JOIN %s l USING (id)
		WHERE l.amount IS DISTINCT FROM o.amount * 100`, old, f.table.Qualified())).Scan(&mismatched))
	assert.Zero(t, mismatched, "every live row holds its retained source amount in cents")
}
