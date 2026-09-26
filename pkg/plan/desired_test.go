package plan_test

import (
	"testing"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefuseDestructiveWithdrawsExecutionAndRehashes(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Disposition = router.DispositionExecute
	report.Statements = []plan.Statement{{
		SQL: "DROP INDEX idx_documents", Destructive: true,
		Backend: router.BackendNative, Disposition: router.DispositionExecute,
		ExecSQL:   []string{"DROP INDEX CONCURRENTLY idx_documents"},
		Execution: planner.ExecutionAutocommit,
		Decisions: []planner.Decision{{SaferSQL: []string{"DROP INDEX CONCURRENTLY idx_documents"}, SaferSQLExecution: planner.ExecutionAutocommit}},
	}, {
		SQL: "ALTER TABLE documents ADD COLUMN body text", Backend: router.BackendNative,
		Disposition: router.DispositionExecute,
	}}
	report.Fingerprint = plan.Fingerprint(report.Statements)
	before := report.Fingerprint
	plan.RefuseDestructive(&report)
	st := report.Statements[0]
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	assert.Equal(t, verdict.ReasonDestructiveChange, report.Reason)
	assert.Equal(t, verdict.ClassByDesign, report.Class)
	assert.Equal(t, router.DispositionRefuse, st.Disposition)
	assert.Empty(t, st.Backend)
	assert.Empty(t, st.ExecSQL)
	assert.Empty(t, st.Execution)
	assert.Empty(t, st.Decisions[0].SaferSQL)
	assert.Empty(t, st.Decisions[0].SaferSQLExecution)
	require.NotNil(t, st.BlockingPassthroughEligible)
	assert.False(t, *st.BlockingPassthroughEligible)
	assert.Equal(t, router.DispositionRefuse, report.Statements[1].Disposition)
	assert.Equal(t, verdict.ReasonDestructiveChange, report.Statements[1].Reason)
	assert.Empty(t, report.Statements[1].Backend)
	assert.NotEqual(t, before, report.Fingerprint)
	assert.Equal(t, plan.Fingerprint(report.Statements), report.Fingerprint)
}

func TestRefuseDestructiveLeavesNondestructivePlanUnchanged(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Disposition = router.DispositionExecute
	report.Statements = []plan.Statement{{
		SQL:     "ALTER TABLE documents ADD COLUMN body text",
		Backend: router.BackendNative, Disposition: router.DispositionExecute,
	}}
	report.Fingerprint = plan.Fingerprint(report.Statements)
	before := report.Statements[0]
	fingerprint := report.Fingerprint
	plan.RefuseDestructive(&report)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	assert.Equal(t, before, report.Statements[0])
	assert.Equal(t, fingerprint, report.Fingerprint)
}

func TestRefuseDestructiveOverridesAggregateRouteRefusal(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Disposition = router.DispositionRefuse
	report.Statements = []plan.Statement{{
		SQL: "ALTER TABLE documents DROP COLUMN body", Destructive: true,
		Disposition: router.DispositionExecute, Backend: router.BackendNative,
	}, {
		SQL:         "ALTER TABLE documents ADD CONSTRAINT ex EXCLUDE USING btree (id WITH =)",
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement, Class: verdict.ClassCapabilityBoundary,
	}}
	plan.RefuseDestructive(&report)
	assert.Equal(t, verdict.ReasonDestructiveChange, report.Reason)
	assert.Equal(t, verdict.ClassByDesign, report.Class)
	assert.Empty(t, report.Owner)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, report.Statements[1].Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, report.Statements[1].Class)
	assert.Equal(t, plan.Fingerprint(report.Statements), report.Fingerprint)
}
