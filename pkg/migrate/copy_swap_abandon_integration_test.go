package migrate

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A data fault travels from the Runner to the front door as a failed
// verdict coded data-fault: the copy met a value the new type will not
// hold — every sku is four characters, varchar(2) holds two — so the Runner
// abandoned the run, and the verdict's detail says so and says the
// corrected statement runs from the beginning. Nothing is left on the
// server: the shadow is gone, the source is unchanged and takes writes
// again, and the corrected statement runs plain to completion with no
// restart acknowledgement.
func TestRunAbandonsACopyAndSwapRunOnADataFault(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN sku TYPE varchar(2)`))
	require.NoError(t, err)

	v, err := Run(t.Context(), f.pool, st, f.options())
	var abandoned *schemachange.AbandonedError
	require.ErrorAs(t, err, &abandoned, "the failure's error is the Runner's abandonment")
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeDataFault), v.Code)
	assert.Equal(t, f.schema+".orders", v.Table)
	assert.Contains(t, v.Detail, "the run was abandoned")
	assert.Contains(t, v.Detail, "the write fence is off the source")
	assert.True(t, v.WritesBlocked, "the copy the run abandoned ran behind its fence")
	assert.True(t, v.Abandoned, "the verdict says the run was discarded")
	assert.NotContains(t, v.Detail, "--restart", "nothing remains for a restart to discard")

	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "the shadow is dropped")
	assert.Equal(t, "text", f.columnType(t, "orders", "sku"), "the source keeps its shape")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
	_, err = f.pool.Exec(t.Context(), f.inSchema(`INSERT INTO {schema}.orders (qty, sku) VALUES (1, 'sku9')`))
	require.NoError(t, err, "the source takes writes")

	corrected, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN sku TYPE varchar(4)`))
	require.NoError(t, err)
	v, err = Run(t.Context(), f.pool, corrected, f.options())
	require.NoError(t, err, "the corrected statement runs plain")
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, "character varying", f.columnType(t, "orders", "sku"), "the gated statement's shape is live")
	assert.EqualValues(t, 2501, f.rowCount(t, "orders"))
}

// The same data fault when the Runner's discard does not finish: a view
// pinned on the shadow makes the abandon's DROP TABLE fail, so the run
// stops with the fault but is not abandoned. The verdict carries the same
// data-fault code, since that is still what the operator corrects, and it
// is the typed field that tells the two apart — abandoned is false, the
// detail offers --restart, and the shadow and the row stand as far as the
// discard got. The restart acknowledgement finishes the discard and runs
// the corrected statement.
func TestRunHoldsACopyAndSwapRunWhoseAbandonFails(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	var pinOnce sync.Once
	pinShadow := func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCopying {
			return nil
		}
		var err error
		pinOnce.Do(func() {
			_, err = f.pool.Exec(ctx, `
				CREATE VIEW `+pgx.Identifier{f.schema, "orders_shadow_reader"}.Sanitize()+` AS
				SELECT id
				FROM `+pgx.Identifier{f.schema, shadow}.Sanitize())
		})
		return err
	}
	opts := f.options()
	opts.CopySwap.OnCheckpoint = pinShadow

	v, err := f.run(t, `ALTER TABLE {schema}.orders ALTER COLUMN sku TYPE varchar(2)`, opts)
	var abandoned *schemachange.AbandonedError
	assert.False(t, errors.As(err, &abandoned), "a discard that failed is not an abandonment")
	assert.True(t, schemachange.IsDataFault(err), "the error still reads as the fault")
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeDataFault), v.Code, "the code names what the operator corrects")
	assert.False(t, v.Abandoned, "the typed field is what tells a held run from an abandoned one")
	assert.True(t, v.WritesBlocked, "the fence went on before the copy met the fault")
	assert.Contains(t, v.Detail, "could not be abandoned")
	assert.Contains(t, v.Detail, "--restart")

	assert.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t).Phase, "the row stands")
	assert.True(t, f.relationExists(t, shadow), "the shadow stands")

	f.exec(t, `DROP VIEW {schema}.orders_shadow_reader`)
	restart := f.options()
	restart.CopySwap.Restart = f.schema + ".orders"
	v, err = f.run(t, `ALTER TABLE {schema}.orders ALTER COLUMN sku TYPE varchar(4)`, restart)
	require.NoError(t, err, "the restart finishes the discard and runs the corrected statement")
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.False(t, v.Abandoned)
	assert.Equal(t, "character varying", f.columnType(t, "orders", "sku"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
}
