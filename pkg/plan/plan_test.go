package plan_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
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
	assert.False(t, st.Destructive, "the router does not know destructiveness")
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
		TableExists:   &exists,
		Disposition:   router.DispositionRefuse,
		Statements: []plan.Statement{
			{
				SQL:         "DROP INDEX t_c_idx",
				Destructive: true,
				Route:       planner.RouteNative,
				Backend:     router.BackendNative,
				Disposition: router.DispositionExecute,
				Decisions: []planner.Decision{{
					Operation: "drop index",
					Route:     planner.RouteNative,
					Reason:    planner.ReasonSaferIdiom,
				}},
				ExecSQL: []string{"DROP INDEX CONCURRENTLY t_c_idx"},
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
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 1,
		"source": "diff",
		"schema": "public",
		"table": "t",
		"table_exists": true,
		"disposition": "refuse",
		"statements": [
			{
				"sql": "DROP INDEX t_c_idx",
				"destructive": true,
				"route": "native",
				"backend": "native",
				"disposition": "execute",
				"decisions": [
					{"operation": "drop index", "route": "native", "reason": "safer-idiom"}
				],
				"exec_sql": ["DROP INDEX CONCURRENTLY t_c_idx"]
			},
			{
				"sql": "ALTER TABLE t NO SUCH THING",
				"route": "refuse",
				"disposition": "refuse",
				"decisions": [
					{"operation": "unrecognized", "route": "refuse", "reason": "unsupported-operation"}
				]
			}
		]
	}`, string(raw))
}

// Optional envelope fields are omitted, not emitted as zero values: an
// alter-source report has no table_exists, and an empty plan serializes
// its statements as [].
func TestReportJSONOmitsUnsetOptionalFields(t *testing.T) {
	r := plan.NewReport(plan.SourceAlter)
	r.Disposition = router.DispositionExecute
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 1,
		"source": "alter",
		"disposition": "execute",
		"statements": []
	}`, string(raw))
}
