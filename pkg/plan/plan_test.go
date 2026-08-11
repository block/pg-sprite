package plan_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

func TestNewReportStampsVersionAndEmptyStatements(t *testing.T) {
	r := plan.NewReport(plan.SourceDiff)
	assert.Equal(t, plan.FormatVersion, r.FormatVersion)
	assert.Equal(t, plan.SourceDiff, r.Source)
	require.NotNil(t, r.Statements)
	assert.Empty(t, r.Statements)
}

func TestFromRoutedMapsEveryRoutedField(t *testing.T) {
	rs := router.Statement{
		Plan: planner.Plan{
			Statement: "CREATE INDEX i ON t (c)",
			Route:     planner.RouteNative,
			Decisions: []planner.Decision{{
				Operation: "create index",
				Route:     planner.RouteNative,
				Reason:    planner.ReasonSaferIdiom,
				SaferSQL:  []string{"CREATE INDEX CONCURRENTLY i ON t (c)"},
			}},
		},
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		ExecSQL:     []string{"CREATE INDEX CONCURRENTLY i ON t (c)"},
	}
	st := plan.FromRouted(rs)
	assert.Equal(t, "CREATE INDEX i ON t (c)", st.SQL)
	assert.Equal(t, planner.RouteNative, st.Route)
	assert.Equal(t, router.BackendNative, st.Backend)
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, rs.Decisions, st.Decisions)
	assert.Equal(t, rs.ExecSQL, st.ExecSQL)
	assert.Equal(t, planner.ExecutionAutocommit, st.Execution,
		"exec_sql carries its execution contract")
	assert.False(t, st.Destructive, "no destructive decision means a non-destructive statement")
}

// Destructive is derived from the classifier's decisions — one destructive
// operation makes the whole statement destructive — so both front doors
// report it identically by construction, whichever one built the report.
func TestFromRoutedDerivesDestructiveFromDecisions(t *testing.T) {
	rs := router.Statement{
		Plan: planner.Plan{
			Statement: "ALTER TABLE t ADD COLUMN c int, DROP COLUMN doomed",
			Route:     planner.RouteNative,
			Decisions: []planner.Decision{
				{Operation: "ADD COLUMN c", Route: planner.RouteNative, Reason: planner.ReasonMetadataOnly},
				{Operation: "DROP COLUMN doomed", Destructive: true, Route: planner.RouteNative, Reason: planner.ReasonMetadataOnly},
			},
		},
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
	}
	st := plan.FromRouted(rs)
	assert.True(t, st.Destructive,
		"one destructive decision makes the statement destructive")
	assert.Empty(t, st.Execution, "no exec_sql means no execution contract")
}

// The JSON shape is the adapter-facing contract: exact keys, exact
// omissions. A consumer pins format_version 1 against this test.
func TestReportJSONShape(t *testing.T) {
	exists := true
	r := plan.Report{
		FormatVersion: plan.FormatVersion,
		Source:        plan.SourceDiff,
		Schema:        "public",
		Table:         "t",
		ServerVersion: "16.4",
		TableExists:   &exists,
		Disposition:   router.DispositionRefuse,
		Statements: []plan.Statement{
			{
				SQL:         "DROP INDEX t_c_idx",
				Kind:        schemadiff.ChangeDropIndex,
				Destructive: true,
				Route:       planner.RouteNative,
				Backend:     router.BackendNative,
				Disposition: router.DispositionExecute,
				Decisions: []planner.Decision{{
					Operation:   "drop index",
					Destructive: true,
					Route:       planner.RouteNative,
					Reason:      planner.ReasonSaferIdiom,
				}},
				ExecSQL:   []string{"DROP INDEX CONCURRENTLY t_c_idx"},
				Execution: planner.ExecutionAutocommit,
			},
			{
				SQL:         "ALTER TABLE t NO SUCH THING",
				Route:       planner.RouteRefuse,
				Disposition: router.DispositionRefuse,
				Decisions: []planner.Decision{{
					Operation: "unrecognized",
					Route:     planner.RouteRefuse,
					Reason:    planner.ReasonUnsupportedOperation,
				}},
			},
		},
	}
	r.Fingerprint = plan.Fingerprint(r.Statements)
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{
		"format_version": 1,
		"source": "diff",
		"schema": "public",
		"table": "t",
		"server_version": "16.4",
		"table_exists": true,
		"disposition": "refuse",
		"fingerprint": %q,
		"statements": [
			{
				"sql": "DROP INDEX t_c_idx",
				"kind": "drop-index",
				"destructive": true,
				"route": "native",
				"backend": "native",
				"disposition": "execute",
				"decisions": [
					{"operation": "drop index", "destructive": true, "route": "native", "reason": "safer-idiom"}
				],
				"exec_sql": ["DROP INDEX CONCURRENTLY t_c_idx"],
				"execution": "autocommit-each-step"
			},
			{
				"sql": "ALTER TABLE t NO SUCH THING",
				"destructive": false,
				"route": "refuse",
				"disposition": "refuse",
				"decisions": [
					{"operation": "unrecognized", "destructive": false, "route": "refuse", "reason": "unsupported-operation"}
				]
			}
		]
	}`, r.Fingerprint), string(raw))
}

// Optional envelope fields are omitted, not emitted as zero values: an
// alter-source report has no table_exists, and an empty plan serializes
// its statements as []. The fingerprint is never optional — an empty plan
// still has a defined identity.
func TestReportJSONOmitsUnsetOptionalFields(t *testing.T) {
	r := plan.NewReport(plan.SourceAlter)
	r.Disposition = router.DispositionExecute
	r.Fingerprint = plan.Fingerprint(r.Statements)
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 1,
		"source": "alter",
		"disposition": "execute",
		"fingerprint": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"statements": []
	}`, string(raw))
}

// The fingerprint serialization is a pinned contract: a fixed statement
// list must hash to this exact digest. If this test fails, the identity
// definition changed and format_version must be bumped.
func TestFingerprintPinnedDigest(t *testing.T) {
	st := plan.Statement{
		SQL:         "ALTER TABLE t DROP COLUMN doomed",
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
	}
	assert.Equal(t,
		"sha256:ec28ea60dfb1894212aa7dbe52ee355ec44b5abd94d0dac550c8e1e64dd76da0",
		plan.Fingerprint([]plan.Statement{st}))
}

// The fingerprint covers what would execute and only that: explanatory
// fields do not change identity; routing, order, and exec_sql do.
func TestFingerprintCoversExecutionNotExplanation(t *testing.T) {
	a := plan.Statement{SQL: "ALTER TABLE t ADD COLUMN c int", Route: planner.RouteNative,
		Backend: router.BackendNative, Disposition: router.DispositionExecute}
	b := plan.Statement{SQL: "ALTER TABLE t DROP COLUMN doomed", Route: planner.RouteNative,
		Backend: router.BackendNative, Disposition: router.DispositionExecute}
	base := plan.Fingerprint([]plan.Statement{a, b})

	explained := a
	explained.Destructive = true
	explained.Kind = schemadiff.ChangeAddColumn
	explained.Decisions = []planner.Decision{{Operation: "ADD COLUMN c", Route: planner.RouteNative}}
	assert.Equal(t, base, plan.Fingerprint([]plan.Statement{explained, b}),
		"decisions, kind, and destructive are explanatory: identity unchanged")

	assert.NotEqual(t, base, plan.Fingerprint([]plan.Statement{b, a}),
		"statement order is part of identity")

	rerouted := a
	rerouted.Route = planner.RouteCopyAndSwap
	rerouted.Backend = router.BackendCopyAndSwap
	assert.NotEqual(t, base, plan.Fingerprint([]plan.Statement{rerouted, b}),
		"a rerouted statement is a different plan")

	rewritten := a
	rewritten.ExecSQL = []string{"CREATE INDEX CONCURRENTLY i ON t (c)"}
	assert.NotEqual(t, base, plan.Fingerprint([]plan.Statement{rewritten, b}),
		"exec_sql is what runs: identity changes with it")
}

// Sources is the closed vocabulary a consumer branches on; the set is
// pinned to format_version 1.
func TestSourcesVocabularyPinned(t *testing.T) {
	assert.Equal(t, []plan.Source{plan.SourceAlter, plan.SourceDiff}, plan.Sources())
}
