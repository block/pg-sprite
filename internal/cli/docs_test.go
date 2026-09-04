package cli

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/lint"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// cliOutputExamplesDoc is the human-facing examples page these tests keep
// honest: its JSON blocks are generated output, not prose, so rebuilding
// them through the real pipeline must reproduce them exactly.
const cliOutputExamplesDoc = "../../docs/cli-output-examples.md"

// docServerVersion is the server the examples were captured against; the
// reports carry it verbatim, so the reproduction must too.
const docServerVersion = "16.14 (Debian 16.14-1.pgdg13+1)"

// usersFacts mirrors the doc's users table (id bigint PRIMARY KEY,
// email text) as the classifier facts a live introspection would yield.
func usersFacts() planner.Facts {
	return planner.Facts{ColumnTypes: map[string]string{"id": "bigint", "email": "text"}}
}

// eventsFacts mirrors the doc's partitioned events table.
func eventsFacts() planner.Facts {
	return planner.Facts{ColumnTypes: map[string]string{"id": "bigint", "created": "date"}}
}

// alterReport rebuilds a migrate --dry-run plan report the way runDryRun
// does: canonicalize, classify with facts, route, convert.
func alterReport(t *testing.T, sql, schema, table string, facts planner.Facts) plan.Report {
	t.Helper()
	canonical, err := statement.Canonical(sql)
	require.NoError(t, err)
	classified, err := planner.Classify(canonical, facts)
	require.NoError(t, err)
	routed := router.Route([]planner.Plan{classified})

	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = schema, table, docServerVersion
	exists := true
	report.TableExists = &exists
	report.Disposition = routed.Disposition
	for _, rs := range routed.Statements {
		ps, err := plan.FromRouted(rs)
		require.NoError(t, err)
		report.Statements = append(report.Statements, ps)
	}
	report.Fingerprint = plan.Fingerprint(report.Statements)
	return report
}

// The doc's --json example outputs are captured pipeline output, not prose:
// rebuilding each one through the same classify-and-route pipeline must
// reproduce the published JSON exactly (up to JSON equivalence). Examples
// of text-only commands are prose and stay outside this check. If this
// fails, regenerate the examples in docs/cli-output-examples.md.
func TestCLIOutputExamplesMatchPipelineOutput(t *testing.T) {
	raw, err := os.ReadFile(cliOutputExamplesDoc)
	require.NoError(t, err)
	blocks := regexp.MustCompile("(?s)```console\n\\$ pg-sprite [^\n]*--json\n(.*?)```").FindAllStringSubmatch(string(raw), -1)
	require.Len(t, blocks, 9, "the doc publishes nine captured --json outputs")

	metadataOnly := alterReport(t, "ALTER TABLE users ADD COLUMN note text",
		"public", "users", usersFacts())
	saferIdiom := alterReport(t, "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)",
		"public", "users", usersFacts())
	executed := verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: "ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)",
		Table:     "public.users",
		Detail:    "the submitted form blocks; pg-sprite ran the safer native sequence instead — all 2 steps committed",
		ExecutedSQL: []string{
			`CREATE UNIQUE INDEX CONCURRENTLY "users_email_key" ON "public"."users" ("email")`,
			`ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_key" UNIQUE USING INDEX "users_email_key"`,
		},
	}
	rewriteRequired := alterReport(t, "ALTER TABLE users ADD COLUMN nickname text UNIQUE",
		"public", "users", usersFacts())
	backendUnavailable := alterReport(t, "ALTER TABLE users ALTER COLUMN id TYPE text",
		"public", "users", usersFacts())

	partitioned := alterReport(t, "CREATE INDEX events_created_idx ON events (created)",
		"public", "events", eventsFacts())
	refused := make([]bool, len(partitioned.Statements))
	for i := range partitioned.Statements {
		cause, err := preflight.RefusesPartitionedParent(16, partitioned.Statements[i].ExecSQL)
		require.NoError(t, err)
		refused[i] = cause != ""
	}
	plan.RefuseUnsupportedPartitionedParent(&partitioned, refused)
	partitioned.Fingerprint = plan.Fingerprint(partitioned.Statements)

	destructive := alterReport(t, "ALTER TABLE users DROP COLUMN email",
		"public", "users", usersFacts())

	lintReport, err := lint.Check("CREATE INDEX users_email_idx ON users (email);\n" +
		"ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);\n")
	require.NoError(t, err)

	diff := plan.NewReport(plan.SourceDiff)
	diff.Schema, diff.Table, diff.ServerVersion = "public", "users", docServerVersion
	exists := true
	diff.TableExists = &exists
	canonical, err := statement.Canonical("ALTER TABLE public.users ADD COLUMN nickname text")
	require.NoError(t, err)
	classified, err := planner.Classify(canonical, usersFacts())
	require.NoError(t, err)
	routed := router.Route([]planner.Plan{classified})
	diff.Disposition = routed.Disposition
	for _, rs := range routed.Statements {
		ps, err := plan.FromRouted(rs)
		require.NoError(t, err)
		ps.Kind = schemadiff.ChangeAddColumn
		diff.Statements = append(diff.Statements, ps)
	}
	diff.Fingerprint = plan.Fingerprint(diff.Statements)

	want := []any{metadataOnly, saferIdiom, executed, rewriteRequired,
		backendUnavailable, partitioned, destructive, lintReport, diff}
	for i, w := range want {
		marshaled, err := json.Marshal(w)
		require.NoError(t, err)
		assert.JSONEq(t, blocks[i][1], string(marshaled),
			"docs/cli-output-examples.md example %d drifted from pipeline output", i+1)
	}
}
