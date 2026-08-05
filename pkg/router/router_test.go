package router_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
)

// classify runs the real classifier so routing tests exercise the same
// plans the CLI produces, not hand-built ones.
func classify(t *testing.T, sql string) planner.Plan {
	t.Helper()
	plan, err := planner.Classify(sql, planner.Facts{})
	require.NoError(t, err)
	return plan
}

func TestRouteNativeExecutesSubmittedForm(t *testing.T) {
	plan := classify(t, "ALTER TABLE t ADD COLUMN age int DEFAULT 0")
	routed := router.Route([]planner.Plan{plan})

	require.Len(t, routed.Statements, 1)
	st := routed.Statements[0]
	assert.Equal(t, router.BackendNative, st.Backend)
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, []string{plan.Statement}, st.ExecSQL,
		"a native statement with no safer sequence executes as submitted")
	assert.Equal(t, router.DispositionExecute, routed.Disposition)
}

func TestRouteNativeExecutesSaferSequence(t *testing.T) {
	plan := classify(t, "CREATE INDEX events_name_idx ON events (name)")
	require.Len(t, plan.Decisions, 1)
	require.NotEmpty(t, plan.Decisions[0].SaferSQL, "classifier must construct the concurrent rewrite")

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.BackendNative, st.Backend)
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, plan.Decisions[0].SaferSQL, st.ExecSQL,
		"the native backend runs the safer sequence, not the submitted form")
}

func TestRouteCopyAndSwapIsUnavailable(t *testing.T) {
	plan := classify(t, "ALTER TABLE t ALTER COLUMN id TYPE bigint")
	require.Equal(t, planner.RouteCopyAndSwap, plan.Route)

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.BackendCopyAndSwap, st.Backend)
	assert.Equal(t, router.DispositionUnavailable, st.Disposition)
	assert.Empty(t, st.ExecSQL, "no literal SQL for a backend that is not implemented")
	assert.Equal(t, router.DispositionUnavailable, routed.Disposition)
}

func TestRouteRefusedStatementHasNoBackend(t *testing.T) {
	plan := classify(t, "ALTER TABLE t ADD CONSTRAINT ex EXCLUDE USING gist (room WITH =)")
	require.Equal(t, planner.RouteRefuse, plan.Route)

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Empty(t, st.Backend)
	assert.Equal(t, router.DispositionRefuse, st.Disposition)
	assert.Empty(t, st.ExecSQL)
	assert.Equal(t, router.DispositionRefuse, routed.Disposition)
}

func TestRouteAggregateIsWorstDisposition(t *testing.T) {
	plans := []planner.Plan{
		classify(t, "ALTER TABLE t ADD COLUMN age int"),
		classify(t, "ALTER TABLE t ALTER COLUMN id TYPE bigint"),
		classify(t, "ALTER TABLE t ADD CONSTRAINT ex EXCLUDE USING gist (room WITH =)"),
	}
	routed := router.Route(plans)

	require.Len(t, routed.Statements, 3)
	assert.Equal(t, router.DispositionExecute, routed.Statements[0].Disposition)
	assert.Equal(t, router.DispositionUnavailable, routed.Statements[1].Disposition)
	assert.Equal(t, router.DispositionRefuse, routed.Statements[2].Disposition)
	assert.Equal(t, router.DispositionRefuse, routed.Disposition,
		"one refusal refuses the whole plan")
}

func TestRouteEmptyPlanExecutes(t *testing.T) {
	routed := router.Route(nil)
	assert.Empty(t, routed.Statements)
	assert.Equal(t, router.DispositionExecute, routed.Disposition,
		"a plan with nothing to do has nothing blocking execution")
}
