package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
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
	require.NoError(t, writeDiffText(&out, palette{}, report, nil))
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
	require.NoError(t, writeDiffText(&out, palette{}, report, nil))
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

// A greenfield plan the create path refuses by shape must not promise the
// create the refusal beneath it withdraws: the leading note says the plan
// is refused, the refused statement carries the create path's own cause as
// a trailing note, and the report counts it refused.
func TestDiffTextGreenfieldRefusedNoteAndCause(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "public", "child", "16.10"
	report.Disposition = router.DispositionRefuse
	missing := false
	report.TableExists = &missing
	sql := `CREATE TABLE "public"."child" PARTITION OF "public"."parent" FOR VALUES IN (1)`
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         sql,
		Route:       planner.RouteNative,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
	})
	causes := []error{executor.ErrPartitionOfUnsupported}

	var out strings.Builder
	require.NoError(t, writeDiffText(&out, palette{}, report, causes))
	text := out.String()
	assert.True(t, strings.HasPrefix(text, "note:\n  the table public.child does not exist — the plan is the full desired\n  schema, and a statement in it is refused, so nothing would be created\n"),
		"the greenfield note must say the plan is refused: %s", text)
	assert.NotContains(t, text, "the plan creates it")
	assert.Contains(t, text, "error[unsupported-statement]:\n  refused — ")
	assert.Contains(t, text, "note:\n  the create path refuses this statement: CREATE TABLE PARTITION OF is not\n  supported by the create path")
	assert.Contains(t, text, "1 statement, 0 steps to run, 1 refused\n")
	assert.NotContains(t, text, "apply:")
	assert.True(t, diffRefused(report))
}

// Without a cause list the refused greenfield statement renders its typed
// refusal alone — the renderer never invents an explanation.
func TestDiffTextGreenfieldRefusedWithoutCauses(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "public", "child", "16.10"
	report.Disposition = router.DispositionRefuse
	missing := false
	report.TableExists = &missing
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `CREATE TABLE "public"."child" ("id" int)`,
		Route:       planner.RouteNative,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
	})

	var out strings.Builder
	require.NoError(t, writeDiffText(&out, palette{}, report, nil))
	assert.NotContains(t, out.String(), "the create path refuses this statement")
	assert.Contains(t, out.String(), "error[unsupported-statement]:\n  refused — ")
}

// The --sql script is copy-pasteable, so a refused statement is annotated
// as refused and emitted as a comment — never as a bare statement a reader
// could run past the gate the engine enforces.
func TestPlanTextCommentsOutRefusedStatement(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "public", "child", "16.10"
	report.Disposition = router.DispositionRefuse
	missing := false
	report.TableExists = &missing
	refusedSQL := `CREATE TABLE "public"."child" PARTITION OF "public"."parent" FOR VALUES IN ('a
b')`
	indexSQL := `CREATE INDEX "child_id_idx" ON "public"."child" ("id")`
	report.Statements = append(report.Statements,
		plan.Statement{
			SQL:         refusedSQL,
			Route:       planner.RouteNative,
			Disposition: router.DispositionRefuse,
			Reason:      verdict.ReasonUnsupportedStatement,
			Decisions: []planner.Decision{{
				Operation: "create table",
				Route:     planner.RouteNative,
				Reason:    planner.ReasonMetadataOnly,
			}},
		},
		plan.Statement{
			SQL:         indexSQL,
			Route:       planner.RouteNative,
			Backend:     router.BackendNative,
			Disposition: router.DispositionExecute,
			ExecSQL:     []string{indexSQL},
			Execution:   planner.ExecutionAutocommit,
			Decisions: []planner.Decision{{
				Operation: "create index",
				Route:     planner.RouteNative,
				Reason:    planner.ReasonMetadataOnly,
			}},
		},
	)
	causes := []error{executor.ErrPartitionOfUnsupported, nil}

	var out strings.Builder
	require.NoError(t, writePlanText(&out, report, causes))
	text := out.String()
	assert.Contains(t, text, "-- native (metadata-only): refused — the engine will not run it\n"+
		"-- the create path refuses this statement: "+executor.ErrPartitionOfUnsupported.Error()+"\n"+
		"-- "+strings.ReplaceAll(refusedSQL, "\n", "\n-- ")+";\n")
	assert.Contains(t, text, "-- native (metadata-only)\n"+indexSQL+";\n")
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(line, "--") {
			continue
		}
		assert.Equal(t, indexSQL+";", line, "the only bare statement is the executable one")
	}
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
	require.NoError(t, writeDiffText(&out, palette{}, report, nil))
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
	require.NoError(t, writeDiffText(&out, palette{}, report, nil))
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
