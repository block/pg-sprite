package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
