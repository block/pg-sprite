package plan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Foreign-key statements stay in the plan so the diff is reviewable, but are
// refused for execution; every other statement keeps its route.
func TestRefuseForeignKeyChangesKeepsStatementsAndRefusesThem(t *testing.T) {
	executable := func(sql string, kind schemadiff.ChangeKind) plan.Statement {
		return plan.Statement{
			SQL: sql, Kind: kind, Route: planner.RouteNative, Backend: router.BackendNative,
			Disposition: router.DispositionExecute, ExecSQL: []string{sql},
		}
	}
	report := plan.NewReport(plan.SourceDiff)
	report.Disposition = router.DispositionExecute
	report.Statements = []plan.Statement{
		executable("ALTER TABLE orders ADD COLUMN note text", schemadiff.ChangeAddColumn),
		executable("ALTER TABLE orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id)", schemadiff.ChangeAddForeignKey),
	}

	plan.RefuseForeignKeyChanges(&report)

	require.Len(t, report.Statements, 2)
	assert.Equal(t, router.DispositionExecute, report.Statements[0].Disposition)
	assert.NotEmpty(t, report.Statements[0].ExecSQL)

	fk := report.Statements[1]
	assert.Equal(t, "ALTER TABLE orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id)", fk.SQL)
	assert.Equal(t, router.DispositionRefuse, fk.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, fk.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, fk.Class)
	assert.Empty(t, fk.Backend)
	assert.Empty(t, fk.ExecSQL)
	require.NotNil(t, fk.BlockingPassthroughEligible)
	assert.False(t, *fk.BlockingPassthroughEligible)

	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, report.Reason)
}
