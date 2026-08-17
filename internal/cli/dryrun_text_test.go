package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/suggest"
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
		Guidance: suggest.GuidanceAddColumnThenConstraint,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, report))
	text := out.String()
	assert.Contains(t, text, "error[rewrite-required]:\n  refused — blocks as written and no online")
	assert.Contains(t, text, "rewrite the change as separate")
	assert.Contains(t, text, "help[add-column-then-constraint]:\n",
		"the typed guidance renders as a help diagnostic with the code as its rule")
	assert.Contains(t, text, "add the plain column first,",
		"the help body carries the guidance prose (wrapped, so assert its head)")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#rewrite-required\n")
	assert.Contains(t, text, "  "+suggestReportURL+"#guidance-guidance\n",
		"guidance links its own doc anchor alongside the diagnostic code anchors")
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
	reasons := append(planner.Reasons(), planner.ReasonAppBreakingRename)
	for _, r := range reasons {
		assert.NotEqual(t, string(r), impactText(r), "reason %s has no prose impact", r)
	}
}
