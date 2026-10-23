package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A retype of the key the router sends to copy-and-swap: the statement
// every test here starts over.
const restartRetype = `ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`

// The restart acknowledgement travels from the front door to the Runner:
// a relation under the shadow's name that no row records, which a plain
// run refuses as shadow-unrecorded, is dropped by the acknowledged run and
// the change completes on a shadow of its own. The acknowledgement names
// the resolved target as the operator typed it.
func TestRunRestartDiscardsALeftoverAndCompletesTheChange(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	f.exec(t, `CREATE TABLE {schema}.`+shadow+` (id bigint PRIMARY KEY)`)
	st, err := statement.ParseOne(f.inSchema(restartRetype))
	require.NoError(t, err)

	refused, err := Run(t.Context(), f.pool, st, f.options())
	require.NoError(t, err, "a refusal is a verdict, not an error")
	require.Equal(t, verdict.OutcomeRefused, refused.Outcome)
	require.Equal(t, verdict.Cause(schemachange.CauseShadowUnrecorded), refused.Cause)
	require.Equal(t, "integer", f.columnType(t, "orders", "qty"), "the refused run executed nothing")

	opts := f.options()
	opts.CopySwap.Restart = f.schema + ".orders"
	v, err := Run(t.Context(), f.pool, st, opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, "bigint", f.columnType(t, "orders", "qty"), "the gated statement's shape is live")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
	assert.False(t, f.relationExists(t, shadow), "the leftover is gone with the run that replaced it")
}

// An acknowledgement that names another table is an error before any
// verdict, and nothing is discarded: the leftover under the shadow's name
// still stands and the source is untouched.
func TestRunRejectsAMismatchedRestartAck(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	f.exec(t, `CREATE TABLE {schema}.`+shadow+` (id bigint PRIMARY KEY)`)
	st, err := statement.ParseOne(f.inSchema(restartRetype))
	require.NoError(t, err)

	opts := f.options()
	opts.CopySwap.Restart = f.schema + ".customers"
	v, err := Run(t.Context(), f.pool, st, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restart acknowledgement")
	assert.Equal(t, verdict.Verdict{}, v, "an error before a verdict carries a zero verdict")
	assert.True(t, f.relationExists(t, shadow), "nothing was discarded")
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "nothing was executed")
}

// An acknowledgement on a statement the router executes natively — an
// instant ADD COLUMN — is an error, not a no-op: there is no copy-and-swap
// run on the table to start over, and the statement is not executed.
func TestRunRejectsARestartAckOnANativelyRoutedStatement(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ADD COLUMN age integer`))
	require.NoError(t, err)

	opts := f.options()
	opts.CopySwap.Restart = f.schema + ".orders"
	v, err := Run(t.Context(), f.pool, st, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "copy-and-swap backend")
	assert.Equal(t, verdict.Verdict{}, v)
	assert.False(t, f.hasColumn(t, "orders", "age"), "nothing was executed")
}
