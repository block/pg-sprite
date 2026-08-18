package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
)

// The text rendering is this renderer's own unit test: the layout below is
// the reviewed shape of the human diff report, so the substitution case is
// asserted whole. The framing names migrate as the executor — diff never
// runs anything — and closes with the diff/sql/apply pointers instead of
// the dry run's dry-run/apply pair.
func TestDiffTextSaferIdiomSubstitution(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionExecute
	exists := true
	report.TableExists = &exists
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE ("email")`,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		Decisions: []planner.Decision{{
			Operation: "add unique constraint",
			Route:     planner.RouteNative,
			Reason:    planner.ReasonSaferIdiom,
		}},
		ExecSQL: []string{
			`CREATE UNIQUE INDEX CONCURRENTLY "u" ON "users" ("email")`,
			`ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE USING INDEX "u"`,
		},
		Execution: planner.ExecutionAutocommit,
	})

	var out strings.Builder
	require.NoError(t, writeDiffText(&out, report))
	assert.Equal(t, `statement 1:
  ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE ("email");

warning[safer-idiom]:
  add unique constraint — holds a blocking lock on the table for the whole
  operation — writes (and for some forms reads) wait until it finishes

note:
  pg-sprite migrate will run a safer online sequence instead:
  1. CREATE UNIQUE INDEX CONCURRENTLY "u" ON "users" ("email");
  2. ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE USING INDEX "u";

note:
  each step commits on its own — not transactionally equivalent, and the
  sequence must not run inside a transaction block

docs:
  `+onlineDDLReferenceURL+`#safer-idiom

plan:
  public.users (PostgreSQL 16.10) — 1 statement, 2 steps to run, 0 refused

diff:
  nothing was executed

sql:
  re-run with --sql to print the plan as an executable SQL script

apply:
  run each statement via pg-sprite migrate --alter '…', which refuses
  blocking forms and substitutes safer online sequences
`, out.String())
}

// A missing table is diff's greenfield case, not an error: the report leads
// with a note that the plan creates the table from the full desired schema.
func TestDiffTextGreenfieldLeadsWithNote(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "public", "widgets", "16.10"
	report.Disposition = router.DispositionExecute
	missing := false
	report.TableExists = &missing
	sql := `CREATE TABLE "public"."widgets" ("id" bigint PRIMARY KEY)`
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         sql,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		ExecSQL:     []string{sql},
		Execution:   planner.ExecutionAutocommit,
	})

	var out strings.Builder
	require.NoError(t, writeDiffText(&out, report))
	text := out.String()
	assert.True(t, strings.HasPrefix(text, "note:\n  the table public.widgets does not exist — the plan creates it from the\n  full desired schema\n"),
		"the greenfield note must lead the report: %s", text)
	assert.Contains(t, text, "statement 1:\n  "+sql+";\n")
	assert.Contains(t, text, "note:\n  runs as written\n")
	assert.Contains(t, text, "plan:\n  public.widgets (PostgreSQL 16.10) — 1 statement, 1 step to run, 0 refused\n")
	assert.NotContains(t, text, "apply:",
		"migrate refuses CREATE TABLE, so a greenfield plan must not point the reader at it")
	assert.Contains(t, text, "sql:\n  re-run with --sql")
	assert.NotContains(t, text, "error[table-not-found]")
}

// A converged table renders a single plan entry — no statements, no
// execution pointers.
func TestDiffTextNoChanges(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionExecute
	exists := true
	report.TableExists = &exists

	var out strings.Builder
	require.NoError(t, writeDiffText(&out, report))
	assert.Equal(t, `plan:
  public.users (PostgreSQL 16.10) — no changes; the live table matches the desired schema
`, out.String())
}

// A plan containing a statement this build cannot execute renders the same
// leading error diagnostic as the dry run, counts it refused, and drops the
// apply pointer — there is nothing migrate would run.
func TestDiffTextRefusedStatementDropsApply(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionUnavailable
	exists := true
	report.TableExists = &exists
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" ALTER COLUMN "id" TYPE text`,
		Route:       planner.RouteCopyAndSwap,
		Backend:     router.BackendCopyAndSwap,
		Disposition: router.DispositionUnavailable,
		Decisions: []planner.Decision{{
			Operation: "alter column type",
			Route:     planner.RouteCopyAndSwap,
			Reason:    planner.ReasonTypeRewrite,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDiffText(&out, report))
	text := out.String()
	assert.Contains(t, text, "error[backend-unavailable]:\n  refused — needs the copy-and-swap backend")
	assert.Contains(t, text, "1 statement, 0 steps to run, 1 refused\n")
	assert.Contains(t, text, "sql:\n  re-run with --sql")
	assert.NotContains(t, text, "apply:")
	assert.True(t, diffRefused(report))
}

// The refusal exit contract keys off the report's disposition, and a
// greenfield plan (missing table) does not refuse — for diff that is the
// normal create-the-table case, unlike the dry run's table-not-found error.
func TestDiffRefusedIgnoresMissingTable(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Disposition = router.DispositionExecute
	missing := false
	report.TableExists = &missing
	assert.False(t, diffRefused(report))

	report.Disposition = router.DispositionRefuse
	assert.True(t, diffRefused(report))
}
