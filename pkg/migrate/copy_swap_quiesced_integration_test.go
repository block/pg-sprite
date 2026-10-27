package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A quiesced acknowledgement that names another table is an error before
// any verdict, and the table is never taken offline: no shadow is built,
// no fence goes on, so the source still takes writes, and its shape is
// unchanged. The acknowledgement is what stops a quiesced declaration left
// on a script from fencing the next table it meets.
func TestRunRejectsAMismatchedQuiescedAck(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)

	opts := f.options()
	opts.CopySwap.Quiesced = f.schema + ".customers"
	v, err := Run(t.Context(), f.pool, st, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quiesced acknowledgement")
	assert.Equal(t, verdict.Verdict{}, v, "an error before a verdict carries a zero verdict")
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "no shadow was built")
	f.exec(t, `INSERT INTO {schema}.orders (qty, note, sku) VALUES (1, 'after', 'sku1')`)
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "nothing was executed")
}
