package router_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
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

// A column or table rename is a brief catalog change for PostgreSQL, but it
// breaks every application instance still using the old name the moment it
// commits. The planner marks it not executable as submitted and routing
// refuses it the same way it refuses a safer idiom with no constructed
// rewrite: the submitted form never becomes executable SQL.
func TestRouteAppBreakingRenameFailsClosed(t *testing.T) {
	for name, sql := range map[string]string{
		"rename column": "ALTER TABLE t RENAME COLUMN a TO b",
		"rename table":  "ALTER TABLE t RENAME TO t2",
	} {
		t.Run(name, func(t *testing.T) {
			plan := classify(t, sql)
			require.Equal(t, planner.RouteNative, plan.Route)
			require.Len(t, plan.Decisions, 1)
			require.Equal(t, planner.ReasonAppBreakingRename, plan.Decisions[0].Reason)
			require.False(t, plan.Decisions[0].ExecutableAsSubmitted())

			routed := router.Route([]planner.Plan{plan})
			st := routed.Statements[0]
			assert.Equal(t, router.BackendNative, st.Backend)
			assert.Equal(t, router.DispositionRewriteRequired, st.Disposition)
			assert.Empty(t, st.ExecSQL, "no executable SQL: the rename only runs under an explicit override")
			assert.Equal(t, router.DispositionRewriteRequired, routed.Disposition)
		})
	}
}

// An index rename is invisible to applications — queries name tables and
// columns, not indexes — so it stays metadata-only and executes as submitted.
func TestRouteIndexRenameExecutes(t *testing.T) {
	plan := classify(t, "ALTER INDEX i RENAME TO i2")
	require.Len(t, plan.Decisions, 1)
	require.Equal(t, planner.ReasonMetadataOnly, plan.Decisions[0].Reason)

	routed := router.Route([]planner.Plan{plan})
	assert.Equal(t, router.DispositionExecute, routed.Statements[0].Disposition)
	assert.Equal(t, []string{"ALTER INDEX i RENAME TO i2"}, routed.Statements[0].ExecSQL,
		"metadata-only: the submitted form is the executable form")
}

func TestRouteKeyWithStorageParametersRunsTheSequence(t *testing.T) {
	plan := classify(t, "ALTER TABLE t ADD CONSTRAINT k UNIQUE (email) WITH (fillfactor = 70)")
	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, []string{
		`CREATE UNIQUE INDEX CONCURRENTLY "k" ON "t" ("email") WITH ("fillfactor" = '70')`,
		`ALTER TABLE "t" ADD CONSTRAINT "k" UNIQUE USING INDEX "k"`,
	}, st.ExecSQL)
}

func TestRouteKeyWithUnrenderableStorageParameterFailsClosed(t *testing.T) {
	// A storage parameter the online sequence cannot carry faithfully gets
	// no sequence: the key must be refused, never built by its blocking
	// form or by a sequence that leaves the parameter out.
	plan := classify(t, "ALTER TABLE t ADD CONSTRAINT k UNIQUE (email) WITH (fillfactor = s.seventy)")
	require.Equal(t, planner.RouteNative, plan.Route)
	require.Len(t, plan.Decisions, 1)
	require.Empty(t, plan.Decisions[0].SaferSQL)

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.DispositionRewriteRequired, st.Disposition)
	assert.Empty(t, st.ExecSQL)
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

func TestRouteTypeRewriteExecutesViaCopyAndSwap(t *testing.T) {
	// A type conversion PostgreSQL cannot relabel is the change the
	// shadow-table engine carries: the gated ALTER retypes the empty shadow
	// and the copy converts each row through the statement's own
	// conversion.
	plan := classify(t, "ALTER TABLE t ALTER COLUMN id TYPE bigint")
	require.Equal(t, planner.RouteCopyAndSwap, plan.Route)

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.BackendCopyAndSwap, st.Backend)
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Empty(t, st.ExecSQL, "the copy-and-swap backend runs the statement itself, not literal native SQL")
	assert.Equal(t, router.DispositionExecute, routed.Disposition)
}

func TestRouteTypeRewriteWithNativeSiblingExecutesViaCopyAndSwap(t *testing.T) {
	// A native-routed operation in the same statement rides along: it runs
	// inside the one gated ALTER on the shadow, so the engine carries the
	// whole statement.
	plan := classify(t, "ALTER TABLE t ALTER COLUMN id TYPE bigint, DROP COLUMN notes")
	require.Equal(t, planner.RouteCopyAndSwap, plan.Route)
	require.Len(t, plan.Decisions, 2)
	require.Equal(t, planner.RouteNative, plan.Decisions[1].Route)

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.BackendCopyAndSwap, st.Backend)
	assert.Equal(t, router.DispositionExecute, st.Disposition)
}

func TestRouteCopyAndSwapReasonsTheEngineDoesNotCarryAreUnavailable(t *testing.T) {
	// Each of these rewrites routes to copy-and-swap, but the engine has no
	// carrying path for it yet; the router marks the statement unavailable
	// so execution refuses it instead of running a change it cannot prove.
	for _, tc := range []struct {
		name   string
		sql    string
		reason planner.Reason
	}{
		{"volatile default", "ALTER TABLE t ADD COLUMN joined timestamptz DEFAULT now()", planner.ReasonVolatileDefault},
		{"stored generated column", "ALTER TABLE t ADD COLUMN total numeric GENERATED ALWAYS AS (qty * price) STORED", planner.ReasonGeneratedStored},
		{"relocation", "ALTER TABLE t SET TABLESPACE pg_default", planner.ReasonRelocation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := classify(t, tc.sql)
			require.Equal(t, planner.RouteCopyAndSwap, plan.Route)
			require.Len(t, plan.Decisions, 1)
			require.Equal(t, tc.reason, plan.Decisions[0].Reason)

			routed := router.Route([]planner.Plan{plan})
			st := routed.Statements[0]
			assert.Equal(t, router.BackendCopyAndSwap, st.Backend)
			assert.Equal(t, router.DispositionUnavailable, st.Disposition)
			assert.Empty(t, st.ExecSQL, "no literal SQL for a change the backend cannot carry")
			assert.Equal(t, router.DispositionUnavailable, routed.Disposition)
		})
	}
}

func TestRouteAddColumnTypeRewriteIsUnavailable(t *testing.T) {
	// The server proved that adding the column rewrites the table for its
	// type alone (a domain with a CHECK). The decision carries the same
	// type-rewrite reason as a retype, but the engine carries only the
	// retype of a column the source has, so the statement is unavailable.
	facts := planner.Facts{AddColumnProofs: map[string]statement.AddColumnProof{"score": statement.AddColumnTypeRewrites}}
	plan, err := planner.Classify("ALTER TABLE t ADD COLUMN score positive_int DEFAULT 1", facts)
	require.NoError(t, err)
	require.Equal(t, planner.RouteCopyAndSwap, plan.Route)
	require.Len(t, plan.Decisions, 1)
	require.Equal(t, planner.ReasonTypeRewrite, plan.Decisions[0].Reason)

	routed := router.Route([]planner.Plan{plan})
	st := routed.Statements[0]
	assert.Equal(t, router.BackendCopyAndSwap, st.Backend)
	assert.Equal(t, router.DispositionUnavailable, st.Disposition)
	assert.Empty(t, st.ExecSQL, "no literal SQL for a change the backend cannot carry")
}

func TestRouteTypeRewriteWithUncarriedSiblingIsUnavailable(t *testing.T) {
	// One operation the engine does not carry makes the whole statement
	// unavailable: the gated ALTER runs as one statement, so the carried
	// type rewrite cannot be split from the sibling.
	plan := classify(t, "ALTER TABLE t ALTER COLUMN id TYPE bigint, ADD COLUMN joined timestamptz DEFAULT now()")
	require.Equal(t, planner.RouteCopyAndSwap, plan.Route)
	require.Len(t, plan.Decisions, 2)

	routed := router.Route([]planner.Plan{plan})
	assert.Equal(t, router.DispositionUnavailable, routed.Statements[0].Disposition)
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

// A route the router does not know is a planner/router version skew. It
// is refused with no backend rather than guessed at: the only way an
// unknown value reaches the backend gate is through this switch, so this
// is the fail-closed line for the whole routing step.
func TestRouteUnknownRouteIsRefused(t *testing.T) {
	plan := classify(t, "ALTER TABLE t ADD COLUMN age int DEFAULT 0")
	plan.Route = planner.Route("bogus")

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
		classify(t, "ALTER TABLE t ADD COLUMN joined timestamptz DEFAULT now()"),
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
