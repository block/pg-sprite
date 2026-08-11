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

func TestRouteSaferIdiomWithoutRewriteFailsClosed(t *testing.T) {
	// ATTACH PARTITION is a safer-idiom decision the planner does not
	// construct a rewrite for: routing must not fall back to the
	// submitted blocking form.
	plan := classify(t, "ALTER TABLE t ATTACH PARTITION p FOR VALUES FROM (1) TO (10)")
	require.Equal(t, planner.RouteNative, plan.Route)
	require.Len(t, plan.Decisions, 1)
	require.Empty(t, plan.Decisions[0].SaferSQL)
	require.False(t, plan.Decisions[0].ExecutableAsSubmitted())

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.BackendNative, st.Backend)
	assert.Equal(t, router.DispositionRewriteRequired, st.Disposition)
	assert.Empty(t, st.ExecSQL, "no executable SQL: running the submitted form would falsify the plan")
	assert.Equal(t, router.DispositionRewriteRequired, routed.Disposition)
}

func TestRouteMultiOpPartialRewriteFailsClosed(t *testing.T) {
	// SET NOT NULL needs a safer sequence, but multi-operation statements
	// carry no rewrites — the submitted form must not run on the strength
	// of the harmless sibling operation.
	plan := classify(t, "ALTER TABLE t ALTER COLUMN age SET NOT NULL, DROP COLUMN b")
	require.Equal(t, planner.RouteNative, plan.Route)
	require.Len(t, plan.Decisions, 2)

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.DispositionRewriteRequired, st.Disposition)
	assert.Empty(t, st.ExecSQL)
}

func TestRouteInlineConstraintAddColumnFailsClosed(t *testing.T) {
	plan := classify(t, "ALTER TABLE t ADD COLUMN c int UNIQUE")
	require.Equal(t, planner.RouteNative, plan.Route)

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.DispositionRewriteRequired, st.Disposition)
	assert.Empty(t, st.ExecSQL,
		"an inline constraint builds its index under ACCESS EXCLUSIVE; the submitted form must not run")
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
		classify(t, "ALTER TABLE t ATTACH PARTITION p FOR VALUES FROM (1) TO (10)"),
		classify(t, "ALTER TABLE t ALTER COLUMN id TYPE bigint"),
		classify(t, "ALTER TABLE t ADD CONSTRAINT ex EXCLUDE USING gist (room WITH =)"),
	}
	routed := router.Route(plans)

	require.Len(t, routed.Statements, 4)
	assert.Equal(t, router.DispositionExecute, routed.Statements[0].Disposition)
	assert.Equal(t, router.DispositionRewriteRequired, routed.Statements[1].Disposition)
	assert.Equal(t, router.DispositionUnavailable, routed.Statements[2].Disposition)
	assert.Equal(t, router.DispositionRefuse, routed.Statements[3].Disposition)
	assert.Equal(t, router.DispositionRefuse, routed.Disposition,
		"one refusal refuses the whole plan")
}

func TestRouteAggregateRanksRewriteRequiredBelowUnavailable(t *testing.T) {
	plans := []planner.Plan{
		classify(t, "ALTER TABLE t ATTACH PARTITION p FOR VALUES FROM (1) TO (10)"),
		classify(t, "ALTER TABLE t ADD COLUMN age int"),
	}
	routed := router.Route(plans)
	assert.Equal(t, router.DispositionRewriteRequired, routed.Disposition,
		"one rewrite-required statement blocks the whole plan")
}

func TestRouteEmptyPlanExecutes(t *testing.T) {
	routed := router.Route(nil)
	assert.Empty(t, routed.Statements)
	assert.Equal(t, router.DispositionExecute, routed.Disposition,
		"a plan with nothing to do has nothing blocking execution")
}

// The backend and disposition vocabularies are closed sets a consumer
// branches on; both are pinned to plan-report format_version 1
// (docs/plan-report.md). A new value here without a format_version bump is
// a contract break.
func TestBackendsVocabularyPinned(t *testing.T) {
	assert.Equal(t, []router.Backend{
		router.BackendNative,
		router.BackendCopyAndSwap,
	}, router.Backends())
}

func TestDispositionsVocabularyPinned(t *testing.T) {
	assert.Equal(t, []router.Disposition{
		router.DispositionExecute,
		router.DispositionRewriteRequired,
		router.DispositionUnavailable,
		router.DispositionRefuse,
	}, router.Dispositions())
}
