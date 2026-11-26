package migrate

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A run that dies after its swap commits, before its done save, leaves a
// cutover row, the new table live, and the retained source on the volume.
// Running the same statement again finishes it. The front door routes a
// type change by the statement, not by the table's current shape, so the
// rerun of
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// on a table whose qty is already bigint still goes to copy-and-swap and
// meets its preflight. That preflight sees the committed swap and asks for
// no disk, so a volume with one byte free admits the rerun, which drops
// the retained source and saves done.
//
// The crash is a real one at the done save: when the first run saves
// cutover, a second session takes the checkpoint row FOR UPDATE and holds
// it, so the swap commits and the done save then fails on its lock
// timeout, leaving the row in cutover.
func TestRunFinishesACommittedSwapOnATightVolume(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`
	st, err := statement.ParseOne(f.inSchema(alter))
	require.NoError(t, err)

	first := f.options()
	first.CopySwap.KeepOld = true
	var holder pgx.Tx
	first.CopySwap.OnCheckpoint = func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCutover {
			return nil
		}
		tx, err := f.pool.Begin(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}
		holder = tx
		_, err = tx.Exec(ctx, `
			SELECT 1
			FROM pgsprite.pgsprite_checkpoint
			FOR UPDATE`)
		return err
	}
	v, err := Run(t.Context(), f.pool, st, first)
	require.Error(t, err, "the done save timed out on the held row")
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	require.NotNil(t, holder)
	require.NoError(t, holder.Rollback(t.Context()))
	require.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t).Phase)
	require.True(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the swap committed and the retained source is on the volume")

	again := f.options()
	again.CopySwap.FreeDiskBytes = 1
	v, err = Run(t.Context(), f.pool, st, again)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)
	assert.False(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the retained source was dropped, freeing its space")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
}
