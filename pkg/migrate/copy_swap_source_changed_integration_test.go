package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here change a table by hand while a copy-and-swap run on it is
// stopped, and then run the stopped statement again. Which table changed
// decides the refusal: the source changing under the run is the
// environment's doing, while a shadow that no longer matches its own row is
// not something the environment explains.

// A run of
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// stopped in verifying leaves its shadow, its fence on the source, and its
// row. Someone then adds a column to the source by hand:
//
//	ALTER TABLE orders ADD COLUMN gift_note text
//
// The shadow was built from the source before that column existed, so the
// stopped run cannot resume, and running the retype again is refused as
// resume-source-changed, an environmental refusal, rather than as a row
// that does not describe its own shadow. The detail names what the run
// leaves and the two ways past it, and nothing is written: the row stays
// in verifying and qty keeps its type. Starting over with the restart
// acknowledgement discards the stopped run and builds a fresh shadow from
// the source as it is now, so the change completes and keeps the column
// added by hand.
func TestRunRefusesToResumeAStoppedRunWhoseSourceChangedByHand(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	retype := f.stopRetypeAtVerifying(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	f.exec(t, `
		ALTER TABLE {schema}.orders
			ADD COLUMN gift_note text`)

	v, err := Run(t.Context(), f.pool, retype, f.options())
	require.NoError(t, err, "a refusal is a verdict, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Equal(t, verdict.Cause(schemachange.CauseResumeSourceChanged), v.Cause)
	assert.True(t, v.WritesBlocked, "the stopped run's fence still stands")
	assert.Contains(t, v.Detail, "shadow table "+f.schema+"."+shadow)
	assert.Contains(t, v.Detail, "the write fence on "+f.schema+".orders")
	assert.Contains(t, v.Detail, "the checkpoint row in phase verifying")
	assert.Contains(t, v.Detail, "--restart "+f.schema+".orders")
	assert.Contains(t, v.Detail, "pg-sprite discard "+f.schema+".orders")
	assert.NotContains(t, v.Detail, "RunOptions", "the detail names the CLI's commands, not the library's options")
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase, "the refusal wrote nothing")
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "nothing was executed")

	restart := f.options()
	restart.CopySwap.Restart = f.schema + ".orders"
	v, err = Run(t.Context(), f.pool, retype, restart)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, "bigint", f.columnType(t, "orders", "qty"), "the restarted change is live")
	assert.True(t, f.hasColumn(t, "orders", "gift_note"), "the column added by hand is kept")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)
}

// The same stopped run, with the source's metadata changed by hand rather
// than its columns or indexes:
//
//	COMMENT ON TABLE orders IS 'changed by hand'
//
// The comment is part of what the swap carries to the live table, so the
// shadow no longer embodies the statement against this source either, and
// the rerun is refused as resume-source-changed with nothing written.
func TestRunRefusesToResumeAStoppedRunWhoseSourceMetadataChangedByHand(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	retype := f.stopRetypeAtVerifying(t)
	f.exec(t, `COMMENT ON TABLE {schema}.orders IS 'changed by hand'`)

	v, err := Run(t.Context(), f.pool, retype, f.options())
	require.NoError(t, err, "a refusal is a verdict, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Equal(t, verdict.Cause(schemachange.CauseResumeSourceChanged), v.Cause)
	assert.Contains(t, v.Detail, "pg-sprite discard "+f.schema+".orders")
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase, "the refusal wrote nothing")
}

// The same stopped run, but the column is added to its shadow by hand
// instead:
//
//	ALTER TABLE <shadow> ADD COLUMN stray text
//
// The source is as the run left it, so nothing outside the engine's own
// relations explains the difference: the row no longer describes the
// shadow under its name, and running the retype again is refused as
// resume-row-mismatch, an invariant violation, with nothing written.
func TestRunRefusesToResumeAStoppedRunWhoseShadowChangedByHand(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	retype := f.stopRetypeAtVerifying(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	f.exec(t, `
		ALTER TABLE {schema}.`+shadow+`
			ADD COLUMN stray text`)

	v, err := Run(t.Context(), f.pool, retype, f.options())
	require.NoError(t, err, "a refusal is a verdict, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.ClassInvariantViolation, v.Class)
	assert.Equal(t, verdict.Cause(schemachange.CauseResumeRowMismatch), v.Cause)
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase, "the refusal wrote nothing")
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "nothing was executed")
}
