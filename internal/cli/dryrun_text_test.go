package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The text rendering is this renderer's own unit test: the layout below is
// the reviewed shape of the human dry-run report, so the substitution case
// is asserted whole.
func TestDryRunTextSaferIdiomSubstitution(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionExecute
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
	require.NoError(t, writeDryRunText(&out, report))
	assert.Equal(t, `statement 1:
  ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE ("email");

warning[safer-idiom]:
  add unique constraint — holds a blocking lock on the table for the whole
  operation — writes (and for some forms reads) wait until it finishes

help:
  pg-sprite will run a safer online sequence instead:
  1. CREATE UNIQUE INDEX CONCURRENTLY "u" ON "users" ("email");
  2. ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE USING INDEX "u";

note:
  each step commits on its own — not transactionally equivalent, and the
  sequence must not run inside a transaction block

docs:
  `+onlineDDLReferenceURL+`#safer-idiom

plan:
  public.users (PostgreSQL 16.10) — 1 statement, 2 steps to run, 0 refused

dry-run:
  nothing was executed

apply:
  re-run without --dry-run
`, out.String())
}

// A statement whose submitted form is already safe runs as written: the
// classification is a note, no substitution prose, and the plan counts its
// single step.
func TestDryRunTextRunAsWritten(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionExecute
	sql := `ALTER TABLE "users" ADD COLUMN "note" text`
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         sql,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		Decisions: []planner.Decision{{
			Operation: "add column",
			Route:     planner.RouteNative,
			Reason:    planner.ReasonMetadataOnly,
		}},
		ExecSQL:   []string{sql},
		Execution: planner.ExecutionAutocommit,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	text := out.String()
	assert.Contains(t, text, "note[metadata-only]:\n  add column — a brief catalog-only change")
	assert.Contains(t, text, "note:\n  runs as written\n")
	assert.Contains(t, text, "docs:\n  "+onlineDDLReferenceURL+"#metadata-only\n")
	assert.Contains(t, text, "plan:\n  public.users (PostgreSQL 16.10) — 1 statement, 1 step to run, 0 refused\n")
	assert.Contains(t, text, "apply:\n  re-run without --dry-run\n")
	assert.NotContains(t, text, "safer online sequence")
}

// A copy-and-swap route in a build without that backend is refused with the
// backend named, no SQL offered to run, and no apply footer.
func TestDryRunTextBackendUnavailable(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionUnavailable
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
	require.NoError(t, writeDryRunText(&out, report))
	text := out.String()
	assert.Contains(t, text, "error[backend-unavailable]:\n  refused — needs the copy-and-swap backend")
	assert.Contains(t, text, "note[type-rewrite]:\n  alter column type — the type conversion forces a full")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#backend-unavailable\n")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#type-rewrite\n")
	assert.Contains(t, text, "plan:\n  public.users (PostgreSQL 16.10) — 1 statement, 0 steps to run, 1 refused\n")
	assert.NotContains(t, text, "pg-sprite will run")
	assert.NotContains(t, text, "apply:")
}

// A safer-idiom decision without a constructed rewrite is refused with the
// manual path named.
func TestDryRunTextRewriteRequired(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionRewriteRequired
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" ADD COLUMN "email" text UNIQUE`,
		Route:       planner.RouteNative,
		Disposition: router.DispositionRewriteRequired,
		Decisions: []planner.Decision{{
			Operation: "add column with inline constraint",
			Route:     planner.RouteNative,
			Reason:    planner.ReasonSaferIdiom,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	text := out.String()
	assert.Contains(t, text, "error[rewrite-required]:\n  refused — blocks as written and no online")
	assert.Contains(t, text, "rewrite the change as separate")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#rewrite-required\n")
	assert.NotContains(t, text, "pg-sprite will run")
	assert.NotContains(t, text, "apply:")
}

// A target-fact refusal carries its typed cause as the rule code, and a
// destructive statement carries the destructive warning alongside it.
func TestDryRunTextRefusalAndDestructive(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" DROP COLUMN "email"`,
		Destructive: true,
		Route:       planner.RouteNative,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedPartitionedParent,
		Decisions: []planner.Decision{{
			Operation:   "drop column",
			Destructive: true,
			Route:       planner.RouteNative,
			Reason:      planner.ReasonMetadataOnly,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	text := out.String()
	assert.Contains(t, text, "error[unsupported-partitioned-parent]:\n  refused — the target is a partitioned table")
	assert.Contains(t, text, "warning[destructive]:\n  this change discards live data or structure")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#unsupported-partitioned-parent\n")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#destructive\n")
	assert.Contains(t, text, "1 refused\n")
}

// A decision taken without live facts carries the conservative-classification
// note so the reader knows introspection could cheapen the plan.
func TestDryRunTextUnverifiedNote(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table = "public", "missing"
	report.Disposition = router.DispositionUnavailable
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "missing" ALTER COLUMN "c" TYPE text`,
		Route:       planner.RouteCopyAndSwap,
		Backend:     router.BackendCopyAndSwap,
		Disposition: router.DispositionUnavailable,
		Decisions: []planner.Decision{{
			Operation:  "alter column type",
			Route:      planner.RouteCopyAndSwap,
			Reason:     planner.ReasonTypeRewrite,
			Unverified: true,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	assert.Contains(t, out.String(), "note:\n  the table was not introspected")
}

// Every reason in the planner's closed set renders as prose, not as its
// token: an operator-facing impact line must never fall through to the
// typed value for a reason this build knows.
func TestDryRunTextImpactCoversAllReasons(t *testing.T) {
	for _, r := range planner.Reasons() {
		assert.NotEqual(t, string(r), impactText(r), "reason %s has no prose impact", r)
	}
}

// Every rule code the dry-run renderer can emit has a matching heading in
// the reference doc, so the docs: line lands on an entry rather than the
// top of the page. The set is closed: every planner reason, every verdict
// reason the dry run can carry, and the renderer's own codes.
func TestDryRunCodesHaveDocAnchors(t *testing.T) {
	raw, err := os.ReadFile("../../docs/postgres-online-ddl-reference.md")
	require.NoError(t, err)
	doc := string(raw)

	codes := []string{
		"rewrite-required",
		"backend-unavailable",
		"destructive",
		"table-not-found",
		string(verdict.ReasonUnsupportedStatement),
		string(verdict.ReasonUnsupportedPartitionedParent),
	}
	for _, r := range planner.Reasons() {
		codes = append(codes, string(r))
	}
	for _, c := range codes {
		assert.Contains(t, doc, "### `"+c+"`\n", "code %s has no reference entry", c)
	}
}

// A planner-level refusal renders the same typed token the run path's
// refusal verdict carries — unsupported-statement — and names the refused
// operation, so the two front doors describe one refusal identically.
func TestDryRunTextPlannerRefusalNamesOperation(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" SET UNLOGGED`,
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Decisions: []planner.Decision{{
			Operation: "unrecognized operation",
			Route:     planner.RouteRefuse,
			Reason:    planner.ReasonUnsupportedOperation,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	text := out.String()
	assert.Contains(t, text, "error[unsupported-statement]:\n  refused — the planner knows no safe path for unrecognized operation")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#unsupported-statement\n")
	assert.NotContains(t, text, "error[refuse]")
}

// A missing target table renders its own error diagnostic, keeps the
// apply footer off, and links the table-not-found reference entry: a plan
// classified from zero facts must not read as ready to apply.
func TestDryRunTextMissingTable(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "nosuchtable", "16.10"
	report.Disposition = router.DispositionExecute
	missing := false
	report.TableExists = &missing
	sql := `ALTER TABLE "nosuchtable" ADD COLUMN "z" integer`
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         sql,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		Decisions: []planner.Decision{{
			Operation: "add column",
			Route:     planner.RouteNative,
			Reason:    planner.ReasonMetadataOnly,
		}},
		ExecSQL:   []string{sql},
		Execution: planner.ExecutionAutocommit,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	text := out.String()
	assert.Contains(t, text, "error[table-not-found]:\n  the target table public.nosuchtable does not exist")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#table-not-found\n")
	assert.Contains(t, text, "dry-run:\n  nothing was executed\n")
	assert.NotContains(t, text, "apply:")
}

// A single-step substitution carries only the transaction-block caveat:
// the multi-step "each step commits on its own" wording would claim a
// sequence that does not exist.
func TestDryRunTextSingleStepSubstitutionNote(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionExecute
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `CREATE INDEX "users_email_idx" ON "users" ("email")`,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		Decisions: []planner.Decision{{
			Operation: "create index",
			Route:     planner.RouteNative,
			Reason:    planner.ReasonSaferIdiom,
		}},
		ExecSQL:   []string{`CREATE INDEX CONCURRENTLY "users_email_idx" ON "users" ("email")`},
		Execution: planner.ExecutionAutocommit,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	text := out.String()
	assert.Contains(t, text, "note:\n  the substituted statement commits on its own and must not run inside a\n  transaction block\n")
	assert.NotContains(t, text, "each step commits on its own")
}

// The plan summary trims the server_version banner to the bare version:
// the packaging suffix belongs in the JSON report, not the one line that
// must stay scannable.
func TestDryRunTextTrimsServerVersionBanner(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table = "public", "users"
	report.ServerVersion = "16.14 (Debian 16.14-1.pgdg13+1)"
	report.Disposition = router.DispositionExecute

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	assert.Contains(t, out.String(), "public.users (PostgreSQL 16.14) —")
	assert.NotContains(t, out.String(), "Debian")
}
