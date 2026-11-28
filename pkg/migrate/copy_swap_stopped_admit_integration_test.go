package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here call the dry run's checks for a copy-and-swap run stopped
// on orders — the retype of qty stopped in verifying — and read the verdict
// each forecasts, which must be the one the apply would give.

// The dry run of an unqualified
//
//	ALTER TABLE orders DROP COLUMN note
//
// resolves orders to the fixture's schema, as Run does before it builds its
// verdict, so the forecast names the statement qualified, as Run's refusal
// does, and a host comparing the two sees one statement.
func TestAdmitStoppedRunNamesAnUnqualifiedStatementQualified(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)

	drop, err := statement.ParseOne(`
		ALTER TABLE orders
			DROP COLUMN note`)
	require.NoError(t, err)
	v, refused, err := AdmitStoppedRun(t.Context(), f.pool, drop, f.schema, "orders")
	require.NoError(t, err)
	require.True(t, refused)
	f.requireStoppedRunRefusal(t, v)
	named, err := statement.ParseOne(v.Statement)
	require.NoError(t, err, v.Statement)
	assert.Equal(t, f.schema, named.Schema(), v.Statement)
	assert.Equal(t, "orders", named.Table(), v.Statement)
}

// A desired plan whose first statement is the stopped retype,
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// resumes the run when it is applied, so its dry run is not refused. A plan
// whose first statement is any other change,
//
//	ALTER TABLE orders ADD COLUMN extra text
//
// meets the stopped run and is refused with copy-and-swap-run-stopped.
func TestAdmitPlanStoppedRunAdmitsOnlyAPlanThatResumesTheStoppedRun(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)

	resume := plan.Report{Schema: f.schema, Table: "orders", Statements: []plan.Statement{{SQL: f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN qty TYPE bigint`)}}}
	_, refused, err := AdmitPlanStoppedRun(t.Context(), f.pool, resume)
	require.NoError(t, err)
	assert.False(t, refused, "the plan's first statement is the stopped run's own")

	other := plan.Report{Schema: f.schema, Table: "orders", Statements: []plan.Statement{{SQL: f.inSchema(`
		ALTER TABLE {schema}.orders
			ADD COLUMN extra text`)}}}
	res, refused, err := AdmitPlanStoppedRun(t.Context(), f.pool, other)
	require.NoError(t, err)
	require.True(t, refused)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome, "%s: %s", res.Reason, res.Detail)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, res.Cause)
}

// A stopped run's row in another format — the state a rolling upgrade
// leaves, which this engine cannot judge — still holds the table while the
// run's shadow stands, so every dry-run check refuses over it, fail closed,
// as Run does: the alter, the empty and the non-empty desired plan, and the
// row security change.
func TestStoppedRunChecksRefuseOverARowOfAnotherFormat(t *testing.T) {
	f := newCopySwapFixtureOwnStore(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)
	f.exec(t, `UPDATE pgsprite.pgsprite_checkpoint SET format_version = format_version + 1`)

	drop, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			DROP COLUMN note`))
	require.NoError(t, err)
	v, refused, err := AdmitStoppedRun(t.Context(), f.pool, drop, f.schema, "orders")
	require.NoError(t, err)
	require.True(t, refused, "alter")
	f.requireStoppedRunRefusal(t, v)

	for name, statements := range map[string][]plan.Statement{
		"empty plan": nil,
		"non-empty plan": {{SQL: f.inSchema(`
			ALTER TABLE {schema}.orders
				ALTER COLUMN qty TYPE bigint`)}},
	} {
		res, refused, err := AdmitPlanStoppedRun(t.Context(), f.pool, plan.Report{Schema: f.schema, Table: "orders", Statements: statements})
		require.NoError(t, err, name)
		require.True(t, refused, name)
		assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, res.Cause, name)
	}

	v, refused, err = AdmitRowSecurityStoppedRun(t.Context(), f.pool, f.schema, "orders")
	require.NoError(t, err)
	require.True(t, refused, "row security")
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, v.Cause)
	assert.Equal(t, f.schema+".orders", v.Table)
}
