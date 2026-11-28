package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here send a change over the retype of qty stopped in verifying
// through the front doors that do not route one statement on the table:
// the desired-state paths that run a statement of their own, and the enum
// value add, which changes a type rather than the table.

// A desired file that declares row security and matches the live table —
// the source, which a run stopped before its swap has not changed — plans
// nothing. Reporting it converged would leave the stopped run's row, shadow,
// and fence standing with nothing to finish them, so it is refused with
// copy-and-swap-run-stopped, as RunDesired refuses an empty plan.
func TestRunDesiredWithRowSecurityRefusesAnEmptyPlanOverAStoppedCopySwapRun(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.exec(t, `ALTER TABLE {schema}.orders ENABLE ROW LEVEL SECURITY`)
	f.exec(t, `CREATE POLICY readers ON {schema}.orders FOR SELECT USING (true)`)
	f.stopRetypeAtVerifying(t)

	desired, err := statement.ParseDesiredWithRowSecurity(`
		CREATE TABLE orders (
			id   bigserial PRIMARY KEY,
			qty  integer NOT NULL,
			note text,
			sku  text
		);
		CREATE INDEX orders_qty_idx ON orders (qty);
		ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
		CREATE POLICY readers ON orders FOR SELECT USING (true);`)
	require.NoError(t, err)
	res, err := RunDesiredWithRowSecurity(t.Context(), f.pool, RowSecurityRequest{Schema: f.schema, Desired: desired}, DefaultOptions())
	require.NoError(t, err, "a refusal is a result, not an error")
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome, "%s: %s", res.Reason, res.Detail)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, res.Reason)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, res.Cause)
	assert.Empty(t, res.Plan.Statements, "the source still has the file's shape")
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase)
}

// A desired file that leaves out orders_qty_idx plans
//
//	DROP INDEX CONCURRENTLY orders_qty_idx
//
// which a reviewed plan runs through its own concurrent drop rather than
// the imperative pipeline. Over the stopped retype it would drop the
// source's index under the run whose shadow was built with it, so it is
// refused with copy-and-swap-run-stopped, pin and acknowledgement given,
// and the index stays.
func TestRunDesiredRefusesAPlannedIndexDropOverAStoppedCopySwapRun(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)

	desired, err := statement.ParseDesired(`
		CREATE TABLE orders (
			id   bigserial PRIMARY KEY,
			qty  integer NOT NULL,
			note text,
			sku  text
		);`)
	require.NoError(t, err)
	reviewed, err := diffplan.Plan(t.Context(), f.pool, diffplan.Request{Schema: f.schema, Desired: desired})
	require.NoError(t, err)
	require.Len(t, reviewed.Statements, 1)
	require.Contains(t, reviewed.Statements[0].SQL, "DROP INDEX")

	res, err := RunDesired(t.Context(), f.pool, DesiredRequest{
		Schema: f.schema, Desired: desired,
		ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: f.schema + ".orders",
	}, DefaultOptions())
	require.NoError(t, err, "a refusal is a result, not an error")
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome, "%s: %s", res.Reason, res.Detail)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, res.Cause)
	require.Len(t, res.Verdicts, 1)
	f.requireStoppedRunRefusal(t, res.Verdicts[0])
	assert.True(t, f.relationExists(t, "orders_qty_idx"), "nothing was executed")
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase)
}

// ALTER TYPE ... ADD VALUE is exempt: it adds a row to the type's label
// list and locks no table, and a label is not part of a table's shape, so
// the stopped run's fingerprints, shadow, and copied rows are unchanged by
// it. Over the retype of qty stopped in verifying, adding a label to the
// type of a column of orders runs, and the stopped retype then resumes and
// finishes.
func TestRunAddsAnEnumValueOverAStoppedCopySwapRun(t *testing.T) {
	f := newCopySwapFixture(t)
	f.exec(t, `CREATE TYPE {schema}.order_state AS ENUM ('open', 'shipped')`)
	f.createOrders(t)
	f.exec(t, `
		ALTER TABLE {schema}.orders
			ADD COLUMN state {schema}.order_state NOT NULL DEFAULT 'open'`)
	retype := f.stopRetypeAtVerifying(t)

	add, err := statement.ParseOne(f.inSchema(`
		ALTER TYPE {schema}.order_state
			ADD VALUE 'returned'`))
	require.NoError(t, err)
	v, err := Run(t.Context(), f.pool, add, DefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.True(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "the stopped run's shadow stands")

	v, err = Run(t.Context(), f.pool, retype, f.options())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, "bigint", f.columnType(t, "orders", "qty"))
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)
}
