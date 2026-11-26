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

// A foreign key the desired file adds or validates keeps its route, so it
// runs online; a foreign key it drops stays in the plan so the diff is
// reviewable, but is refused for execution.
func TestRefuseForeignKeyDropsKeepsAddsAndValidationsRouted(t *testing.T) {
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
		executable("ALTER TABLE orders VALIDATE CONSTRAINT orders_account_id_fkey", schemadiff.ChangeValidateForeignKey),
		executable("ALTER TABLE orders DROP CONSTRAINT orders_coupon_id_fkey", schemadiff.ChangeDropForeignKey),
	}

	plan.RefuseForeignKeyDrops(&report)

	require.Len(t, report.Statements, 4)
	for _, st := range report.Statements[:3] {
		assert.Equal(t, router.DispositionExecute, st.Disposition, st.SQL)
		assert.Equal(t, []string{st.SQL}, st.ExecSQL, st.SQL)
	}

	drop := report.Statements[3]
	assert.Equal(t, "ALTER TABLE orders DROP CONSTRAINT orders_coupon_id_fkey", drop.SQL)
	assert.Equal(t, router.DispositionRefuse, drop.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, drop.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, drop.Class)
	assert.Empty(t, drop.Backend)
	assert.Empty(t, drop.ExecSQL)
	require.NotNil(t, drop.BlockingPassthroughEligible)
	assert.False(t, *drop.BlockingPassthroughEligible)

	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, report.Reason)
}
