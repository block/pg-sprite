package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
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
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	assert.Equal(t, `statement 1:
  ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE ("email");

warning[safer-idiom]:
  add unique constraint — holds a blocking lock on the table for the whole
  operation — writes (and for some forms reads) wait until it finishes

note:
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
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	assert.Contains(t, text, "note[metadata-only]:\n  add column — a brief catalog-only change")
	assert.Contains(t, text, "note:\n  runs as written\n")
	assert.Contains(t, text, "docs:\n  "+onlineDDLReferenceURL+"#metadata-only\n")
	assert.Contains(t, text, "plan:\n  public.users (PostgreSQL 16.10) — 1 statement, 1 step to run, 0 refused\n")
	assert.Contains(t, text, "apply:\n  re-run without --dry-run\n")
	assert.NotContains(t, text, "safer online sequence")
}

// A CREATE INDEX on a partitioned parent renders as one step that names the
// partition-by-partition build: the statement does not run as written, and
// the note says how an interrupted run continues.
func TestDryRunTextPartitionedIndexBuild(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "events", "16.10"
	report.Disposition = router.DispositionExecute
	sql := `CREATE INDEX "events_created_idx" ON "public"."events" ("created")`
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         sql,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		ExecSQL:     []string{sql},
		Execution:   planner.ExecutionPartitionedIndexBuild,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	flat := strings.Join(strings.Fields(text), " ")
	assert.Contains(t, flat, "will build the index partition by partition instead of running the statement")
	assert.Contains(t, flat, "an interrupted run is resumed by running the same statement again")
	assert.Contains(t, text, "plan:\n  public.events (PostgreSQL 16.10) — 1 statement, 1 step to run, 0 refused\n")
	assert.NotContains(t, text, "runs as written")
}

// A dry run of a key on a partitioned table names the partitioned key build:
// the statement does not run as written, and the note says how an
// interrupted run continues.
func TestDryRunTextPartitionedKeyBuild(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "events", "16.10"
	report.Disposition = router.DispositionExecute
	sql := `ALTER TABLE "public"."events" ADD CONSTRAINT "events_pkey" PRIMARY KEY ("id", "created")`
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         sql,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		ExecSQL:     []string{sql},
		Execution:   planner.ExecutionPartitionedKeyBuild,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	flat := strings.Join(strings.Fields(text), " ")
	assert.Contains(t, flat, "will add the constraint partition by partition instead of running the statement")
	assert.Contains(t, flat, "an interrupted run is resumed by running the same statement again")
	assert.Contains(t, text, "plan:\n  public.events (PostgreSQL 16.10) — 1 statement, 1 step to run, 0 refused\n")
	assert.NotContains(t, text, "runs as written")
}

// A dry run of a foreign key on a partitioned table names the partitioned
// foreign key build: the statement does not run as written, the parent's
// step holds ACCESS EXCLUSIVE on the referenced table, and the note says how
// an interrupted run continues.
func TestDryRunTextPartitionedForeignKeyBuild(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "app", "events", "16.10"
	report.Disposition = router.DispositionExecute
	sql := `ALTER TABLE "app"."events" ADD CONSTRAINT "events_account_fk" FOREIGN KEY ("account_id") REFERENCES "app"."accounts" ("id")`
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         sql,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		ExecSQL:     []string{sql},
		Execution:   planner.ExecutionPartitionedForeignKeyBuild,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	flat := strings.Join(strings.Fields(text), " ")
	assert.Contains(t, flat, "will add the foreign key partition by partition instead of running the statement")
	assert.Contains(t, flat, "holds ACCESS EXCLUSIVE on the referenced table")
	assert.Contains(t, flat, "an interrupted run is resumed by running the same statement again")
	assert.Contains(t, text, "plan:\n  app.events (PostgreSQL 16.10) — 1 statement, 1 step to run, 0 refused\n")
	assert.NotContains(t, text, "runs as written")
}

// A copy-and-swap route the engine carries renders as one step: no literal
// SQL is listed, the note says what the run does, and the apply footer
// invites the real run.
func TestDryRunTextCopyAndSwapExecutes(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionExecute
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" ALTER COLUMN "id" TYPE text`,
		Route:       planner.RouteCopyAndSwap,
		Backend:     router.BackendCopyAndSwap,
		Disposition: router.DispositionExecute,
		Decisions: []planner.Decision{{
			Operation: "alter column type",
			Route:     planner.RouteCopyAndSwap,
			Reason:    planner.ReasonTypeRewrite,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	assert.Contains(t, text, "note[type-rewrite]:\n  alter column type — converting or checking every row for the column's\n  type forces a full")
	assert.Contains(t, text, "note:\n  pg-sprite will run it as a copy-and-swap: build a shadow table")
	assert.Contains(t, text, "(needs --copy-and-swap to\n  open the preview route, --free-disk, and logical decoding on the server;", "the note names every flag the real run needs, the preview gate first")
	assert.Contains(t, text, "is not part of this dry run", "the note says the route's preflight is read at the run, not here")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#type-rewrite\n")
	assert.Contains(t, text, "plan:\n  public.users (PostgreSQL 16.10) — 1 statement, 1 step to run, 0 refused\n")
	assert.Contains(t, text, "apply:\n  re-run without --dry-run\n")
	assert.NotContains(t, text, "error[")
	assert.NotContains(t, text, "runs as written")
}

// A copy-and-swap route for an operation the engine does not carry yet is
// refused with the backend named, no SQL offered to run, and no apply
// footer.
func TestDryRunTextBackendUnavailable(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionUnavailable
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" ADD COLUMN "joined" timestamptz DEFAULT now()`,
		Route:       planner.RouteCopyAndSwap,
		Backend:     router.BackendCopyAndSwap,
		Disposition: router.DispositionUnavailable,
		Decisions: []planner.Decision{{
			Operation: "add column joined",
			Route:     planner.RouteCopyAndSwap,
			Reason:    planner.ReasonVolatileDefault,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	assert.Contains(t, text, "error[backend-unavailable]:\n  refused — needs the copy-and-swap backend")
	assert.Contains(t, text, "note[volatile-default]:\n  add column joined — a default computed for every row forces a full table")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#backend-unavailable\n")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#volatile-default\n")
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
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	assert.Contains(t, text, "error[rewrite-required]:\n  refused — blocks as written and no online")
	assert.Contains(t, text, "rewrite the change as separate")
	assert.Contains(t, text, "help[add-column-then-constraint]:\n",
		"the typed guidance renders as a help diagnostic with the code as its rule")
	assert.Contains(t, text, "add the plain column first,",
		"the help body carries the guidance prose (wrapped, so assert its head)")
	assert.Greater(t, strings.Index(text, "help[add-column-then-constraint]:"),
		strings.Index(text, "note[safer-idiom]:"),
		"the manual path trails the finding that explains it — help: after the diagnosis, compiler style")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#rewrite-required\n")
	assert.Contains(t, text, "  "+suggestReportURL+"#add-column-then-constraint\n",
		"guidance links its own per-code doc anchor alongside the diagnostic code anchors")
	assert.NotContains(t, text, "pg-sprite will run")
	assert.NotContains(t, text, "apply:")
}

// A rename is refused under the same rewrite-required code, but the refusal
// text names the application break rather than a blocking lock, and the help
// names the rollout the application can follow plus the override that runs
// the rename as written.
func TestDryRunTextAppBreakingRenameRefused(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionRewriteRequired
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" RENAME COLUMN "name" TO "full_name"`,
		Route:       planner.RouteNative,
		Disposition: router.DispositionRewriteRequired,
		Decisions: []planner.Decision{{
			Operation: "rename column",
			Route:     planner.RouteNative,
			Reason:    planner.ReasonAppBreakingRename,
		}},
		Guidance: suggest.GuidanceExpandContractColumn,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	assert.Contains(t, text, "error[rewrite-required]:\n  refused — breaks every running application instance")
	assert.Contains(t, text, "run it as written with --force",
		"the refusal names the override, since the rename itself is a valid change the operator may accept (wrapped, so assert its head)")
	assert.NotContains(t, text, "no online replacement could be constructed",
		"a rename is not a blocking-lock refusal; the text must not say it is")
	assert.Contains(t, text, "help[expand-contract-column]:\n")
	assert.Greater(t, strings.Index(text, "help[expand-contract-column]:"),
		strings.Index(text, "note[app-breaking-rename]:"),
		"the manual path trails the finding that explains it")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#rewrite-required\n")
	assert.Contains(t, text, "  "+suggestReportURL+"#expand-contract-column\n")
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
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{{
			Operation:   "drop column",
			Destructive: true,
			Route:       planner.RouteNative,
			Reason:      planner.ReasonMetadataOnly,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	assert.Contains(t, text, "error[unsupported-partitioned-parent]:\n  refused — the target is a partitioned table")
	assert.Contains(t, text, "note:\n  refusal class: capability-boundary\n",
		"the text renderer names the routing class the JSON report carries")
	assert.NotContains(t, text, "; owner:", "a capability-boundary refusal has no owner to name")
	assert.Contains(t, text, "warning[destructive]:\n  this change discards live data or structure")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#unsupported-partitioned-parent\n")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#destructive\n")
	assert.Contains(t, text, "1 refused\n")
}

// A dropped index of a partitioned table has no concurrent form, so the
// refusal names the plain drop it is and the flags that run it, not the
// partitioned-table refusal every other statement gets.
func TestDryRunTextPartitionedIndexDropNamesThePlainDrop(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "app", "events", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `DROP INDEX app.events_name_idx`,
		Kind:        schemadiff.ChangeDropIndex,
		Destructive: true,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedPartitionedParent,
		Class:       verdict.ClassByDesign,
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := strings.Join(strings.Fields(out.String()), " ")
	assert.Contains(t, text, "PostgreSQL has no concurrent drop for an index of a partitioned table; "+
		"the drop is this plain DROP INDEX, which takes ACCESS EXCLUSIVE on the parent, every partition, "+
		"and every partition's index — run it with pg-sprite migrate --alter, --accept-blocking, "+
		"and --allow-destructive naming the table")
	assert.NotContains(t, text, "refused — the target is a partitioned table")

	// A desired plan that discloses the blocking drop names the flags a
	// pinned desired-state run takes it with.
	report.Statements[0].Execution = planner.ExecutionBlockingIndexDrop
	out.Reset()
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text = strings.Join(strings.Fields(out.String()), " ")
	assert.Contains(t, text, "every partition's index — a run takes it only with --expect-fingerprint, "+
		"--allow-destructive and --accept-blocking naming the table, and an explicit --statement-timeout longer than --lock-timeout")
}

// A decision taken without live facts carries the conservative-classification
// note so the reader knows introspection could cheapen the plan.
func TestDryRunTextUnverifiedNote(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table = "public", "missing"
	report.Disposition = router.DispositionExecute
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "missing" ALTER COLUMN "c" TYPE text`,
		Route:       planner.RouteCopyAndSwap,
		Backend:     router.BackendCopyAndSwap,
		Disposition: router.DispositionExecute,
		Decisions: []planner.Decision{{
			Operation:  "alter column type",
			Route:      planner.RouteCopyAndSwap,
			Reason:     planner.ReasonTypeRewrite,
			Unverified: true,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
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
		string(verdict.ReasonDeclarativeForeignKey),
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
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{{
			Operation: "unrecognized operation",
			Route:     planner.RouteRefuse,
			Reason:    planner.ReasonUnsupportedOperation,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	assert.Contains(t, text, "error[unsupported-statement]:\n  refused — the planner knows no safe path for unrecognized operation")
	assert.Contains(t, text, "note:\n  refusal class: capability-boundary\n")
	assert.Contains(t, text, "  "+onlineDDLReferenceURL+"#unsupported-statement\n")
	assert.NotContains(t, text, "error[refuse]")
}

// A refusal because views read the changed column names those views in
// the refusal itself, rather than the generic no-safe-path sentence, and
// the help line says what to do about them.
func TestDryRunTextDependentViewRefusalNamesTheViews(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "app", "users", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         "ALTER TABLE app.users DROP COLUMN email",
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{{
			Operation:      "DROP COLUMN email",
			Destructive:    true,
			Route:          planner.RouteRefuse,
			Reason:         planner.ReasonDependentView,
			DependentViews: []string{`"app"."active_users"`, `"reports"."mail"`},
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := strings.Join(strings.Fields(out.String()), " ")
	assert.Contains(t, text, `error[unsupported-statement]: refused — DROP COLUMN email changes a column read by views "app"."active_users", "reports"."mail", and PostgreSQL refuses to retype or drop a column while a view reads it`)
	assert.Contains(t, text, "help: drop or redefine those views first, then re-plan")
	assert.Contains(t, text, onlineDDLReferenceURL+"#dependent-view")
	assert.NotContains(t, text, "knows no safe path")
}

// A refusal because views read the changed column calls a materialized
// view by its kind, and the help gives the manual path the change takes
// with the views out of the way: for an in-place type change, recreating
// them around it in one transaction, where a materialized view also runs
// its query to fill itself again.
func TestDryRunTextDependentViewRefusalNamesMaterializedViews(t *testing.T) {
	for name, tc := range map[string]struct {
		fix        planner.DependentFix
		readers    []string
		views      []string
		refusal    string
		help       string
		notContain string
	}{
		"a materialized view on an in-place type change": {
			fix:     planner.DependentFixRecreateAroundChange,
			views:   []string{`"app"."mail"`},
			readers: []string{`"app"."mail_top"`, `"app"."mail"`},
			refusal: `ALTER COLUMN email TYPE varchar(100) changes a column read by materialized view "app"."mail", and PostgreSQL refuses`,
			help: "help: this type change runs in place, without rewriting the table, so in one transaction drop those materialized views, run the change, and recreate them; " +
				"recreating a materialized view runs its query to fill it again while the transaction blocks the table; " +
				`drop the views in this order: view "app"."mail_top", then materialized view "app"."mail", and recreate them in reverse; ` +
				"a dropped view loses its grants, comment, owner, options such as security_barrier, rules, INSTEAD OF triggers and column defaults, and a materialized view its indexes too, so recreate those with each view",
		},
		"a view and a materialized view on a type change that rewrites the table": {
			fix:        planner.DependentFixExpandContract,
			views:      []string{`"app"."active_users"`, `"app"."mail"`},
			refusal:    `ALTER COLUMN email TYPE varchar(100) changes a column read by view "app"."active_users" and materialized view "app"."mail", and PostgreSQL refuses`,
			help:       "point those views and materialized views at it and switch reads to it, then drop the old column once nothing uses it",
			notContain: "fill it again",
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := plan.NewReport(plan.SourceAlter)
			report.Schema, report.Table, report.ServerVersion = "app", "users", "16.10"
			report.Disposition = router.DispositionRefuse
			report.Statements = append(report.Statements, plan.Statement{
				SQL:         "ALTER TABLE app.users ALTER COLUMN email TYPE varchar(100)",
				Route:       planner.RouteRefuse,
				Disposition: router.DispositionRefuse,
				Reason:      verdict.ReasonUnsupportedStatement,
				Class:       verdict.ClassCapabilityBoundary,
				Decisions: []planner.Decision{{
					Operation:                  "ALTER COLUMN email TYPE varchar(100)",
					Route:                      planner.RouteRefuse,
					Reason:                     planner.ReasonDependentView,
					DependentViews:             tc.views,
					DependentMaterializedViews: []string{`"app"."mail"`},
					DependentViewDropOrder:     tc.readers,
					DependentFix:               tc.fix,
				}},
			})

			var out strings.Builder
			require.NoError(t, writeDryRunText(&out, palette{}, report))
			text := strings.Join(strings.Fields(out.String()), " ")
			assert.Contains(t, text, "error[unsupported-statement]: refused — "+tc.refusal)
			assert.Contains(t, text, tc.help)
			assert.NotContains(t, text, "drop or redefine those")
			if tc.notContain != "" {
				assert.NotContains(t, text, tc.notContain)
			}
		})
	}
}

// A refusal because row-level security policies read the changed column
// names those policies in the refusal itself, and the help line gives
// the manual path for the change: for an in-place type change, recreating
// them in the same transaction, so the table is never left without them.
func TestDryRunTextDependentPolicyRefusalNamesThePolicies(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "app", "documents", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         "ALTER TABLE app.documents ALTER COLUMN owner_id TYPE text",
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{{
			Operation:         "ALTER COLUMN owner_id TYPE text",
			Route:             planner.RouteRefuse,
			Reason:            planner.ReasonDependentPolicy,
			DependentPolicies: []string{`"own_rows" ON "app"."documents"`},
			DependentFix:      planner.DependentFixRecreateAroundChange,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := strings.Join(strings.Fields(out.String()), " ")
	assert.Contains(t, text, `error[unsupported-statement]: refused — ALTER COLUMN owner_id TYPE text changes a column read by policy "own_rows" ON "app"."documents", and PostgreSQL refuses to retype or drop a column while a policy reads it`)
	assert.Contains(t, text, "help: this type change runs in place, without rewriting the table, so in one transaction drop those policies, run the change, and recreate them")
	assert.Contains(t, text, onlineDDLReferenceURL+"#dependent-policy")
	assert.NotContains(t, text, "knows no safe path")
	assert.NotContains(t, text, "those views")
}

// A refusal because a trigger, a generated column, or a partition key
// depends on the changed column names those objects in the refusal
// itself, and the help line gives the manual path for each. A second kind
// of dependent that also blocks the change is named in its own help line.
func TestDryRunTextHiddenDependentRefusalsNameTheObjects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision planner.Decision
		refusal  string
		help     string
		anchor   string
	}{
		{
			name: "trigger",
			decision: planner.Decision{
				Operation:         "ALTER COLUMN status TYPE varchar(64)",
				Reason:            planner.ReasonDependentTrigger,
				DependentTriggers: []string{`"audit_status" ON "app"."orders"`},
				DependentFix:      planner.DependentFixRecreateAroundChange,
			},
			refusal: `ALTER COLUMN status TYPE varchar(64) changes a column named by trigger "audit_status" ON "app"."orders", and PostgreSQL refuses to retype or drop a column while a trigger's UPDATE OF list or WHEN condition names it`,
			help:    "help: this type change runs in place, without rewriting the table, so in one transaction drop those triggers, run the change, and recreate them",
			anchor:  "#dependent-trigger",
		},
		{
			name: "trigger and generated column",
			decision: planner.Decision{
				Operation:                 "ALTER COLUMN status TYPE varchar(64)",
				Reason:                    planner.ReasonDependentTrigger,
				DependentTriggers:         []string{`"audit_status" ON "app"."orders"`},
				DependentGeneratedColumns: []string{`"app"."orders"."status_upper"`},
				DependentFix:              planner.DependentFixRecreateAroundChange,
			},
			refusal: `ALTER COLUMN status TYPE varchar(64) changes a column named by trigger "audit_status" ON "app"."orders", and PostgreSQL refuses to retype or drop a column while a trigger's UPDATE OF list or WHEN condition names it`,
			help:    `help: the change is also blocked by generated column "app"."orders"."status_upper": ` + planner.GeneratedColumnHelp,
			anchor:  "#dependent-trigger",
		},
		{
			name: "publication",
			decision: planner.Decision{
				Kind:                  statement.OpDropColumn,
				Operation:             "DROP COLUMN amount",
				Reason:                planner.ReasonDependentPublication,
				DependentPublications: []string{`"orders_pub"`},
			},
			refusal: `DROP COLUMN amount changes a column named by publication "orders_pub", and PostgreSQL refuses to retype or drop a column while a publication's column list or row filter names it`,
			help:    "help: " + planner.PublicationHelp(statement.OpDropColumn),
			anchor:  "#dependent-publication",
		},
		{
			name: "generated column",
			decision: planner.Decision{
				Operation:                 "DROP COLUMN amount",
				Reason:                    planner.ReasonDependentGeneratedColumn,
				DependentGeneratedColumns: []string{`"app"."orders"."total_cents"`, `"app"."orders"."total_tax"`},
			},
			refusal: `DROP COLUMN amount changes a column read by generated columns "app"."orders"."total_cents", "app"."orders"."total_tax", and PostgreSQL refuses to retype or drop a column while a generated column reads it, except that PostgreSQL 14 drops the generated column along with a dropped column`,
			help:    "help: " + planner.GeneratedColumnHelp,
			anchor:  "#dependent-generated-column",
		},
		{
			name: "partition key",
			decision: planner.Decision{
				Operation:          "ALTER COLUMN created_at TYPE timestamptz",
				Reason:             planner.ReasonPartitionKey,
				PartitionKeyTables: []string{`"app"."orders"`},
			},
			refusal: `ALTER COLUMN created_at TYPE timestamptz changes a column in the partition key of "app"."orders", and PostgreSQL never retypes or drops a partition key column`,
			help:    "help: " + planner.PartitionKeyHelp,
			anchor:  "#partition-key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := plan.NewReport(plan.SourceAlter)
			report.Schema, report.Table, report.ServerVersion = "app", "orders", "16.10"
			report.Disposition = router.DispositionRefuse
			d := tc.decision
			d.Route = planner.RouteRefuse
			report.Statements = append(report.Statements, plan.Statement{
				SQL:         "ALTER TABLE app.orders " + d.Operation,
				Route:       planner.RouteRefuse,
				Disposition: router.DispositionRefuse,
				Reason:      verdict.ReasonUnsupportedStatement,
				Class:       verdict.ClassCapabilityBoundary,
				Decisions:   []planner.Decision{d},
			})

			var out strings.Builder
			require.NoError(t, writeDryRunText(&out, palette{}, report))
			text := strings.Join(strings.Fields(out.String()), " ")
			assert.Contains(t, text, "error[unsupported-statement]: refused — "+tc.refusal)
			assert.Contains(t, text, tc.help)
			assert.Contains(t, text, onlineDDLReferenceURL+tc.anchor)
			assert.NotContains(t, text, "knows no safe path")
		})
	}
}

// A refusal because a NOT NULL column with no default would leave the
// table's rows holding NULL says so, and the help line gives the two ways
// to add the column, once for a statement that adds two such columns.
func TestDryRunTextNotNullWithoutDefaultRefusalGivesTheManualPath(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "app", "customers", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         "ALTER TABLE app.customers ADD COLUMN tier text NOT NULL, ADD COLUMN plan text NOT NULL",
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{{
			Operation: "ADD COLUMN tier",
			Route:     planner.RouteRefuse,
			Reason:    planner.ReasonNotNullWithoutDefault,
		}, {
			Operation: "ADD COLUMN plan",
			Route:     planner.RouteRefuse,
			Reason:    planner.ReasonNotNullWithoutDefault,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := strings.Join(strings.Fields(out.String()), " ")
	assert.Contains(t, text, "error[unsupported-statement]: refused — ADD COLUMN tier adds a NOT NULL column with no default to a table that holds rows, and PostgreSQL refuses it because every existing row would hold NULL")
	assert.Contains(t, text, "note[not-null-without-default]: ADD COLUMN plan — adds a NOT NULL column with no default")
	assert.Equal(t, 1, strings.Count(text, "help: "+planner.NotNullWithoutDefaultHelp))
	assert.Contains(t, text, onlineDDLReferenceURL+"#not-null-without-default")
	assert.NotContains(t, text, "knows no safe path")
}

// A NOT NULL column that takes its NOT NULL from its domain gets help that
// names the domain and says to use its base type, since neither manual
// path of a spelled NOT NULL exists for it; a spelled NOT NULL beside it
// still gets the shared help.
func TestDryRunTextNotNullFromItsDomainNamesTheDomain(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "app", "customers", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         "ALTER TABLE app.customers ADD COLUMN qty app.positive_qty, ADD COLUMN plan text NOT NULL",
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{{
			Operation:       "ADD COLUMN qty",
			Route:           planner.RouteRefuse,
			Reason:          planner.ReasonNotNullWithoutDefault,
			NotNullFromType: "app.positive_qty",
		}, {
			Operation: "ADD COLUMN plan",
			Route:     planner.RouteRefuse,
			Reason:    planner.ReasonNotNullWithoutDefault,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := strings.Join(strings.Fields(out.String()), " ")
	assert.Equal(t, 1, strings.Count(text, "help: "+planner.NotNullFromTypeHelp("app.positive_qty")))
	assert.Equal(t, 1, strings.Count(text, "help: "+planner.NotNullWithoutDefaultHelp))
}

// An added column of a domain with constraints needs a backend this build
// does not carry; the dry run says which domain forces the rewrite and the
// base-type path that avoids it.
func TestDryRunTextConstrainedDomainNamesTheBaseTypePath(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "app", "customers", "16.10"
	report.Disposition = router.DispositionUnavailable
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         "ALTER TABLE app.customers ADD COLUMN qty app.positive_qty DEFAULT 1",
		Route:       planner.RouteCopyAndSwap,
		Disposition: router.DispositionUnavailable,
		Backend:     router.BackendCopyAndSwap,
		Decisions: []planner.Decision{{
			Operation:       "ADD COLUMN qty",
			Route:           planner.RouteCopyAndSwap,
			Reason:          planner.ReasonTypeRewrite,
			ConstrainedType: "app.positive_qty",
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := strings.Join(strings.Fields(out.String()), " ")
	assert.Equal(t, 1, strings.Count(text, "help: "+planner.ConstrainedTypeHelp("app.positive_qty")))
}

// The refusal sentence explains the first refused operation, the same one
// the run's verdict explains, even when a later operation is refused
// because views read its column; that operation's views still appear in
// its own diagnostic.
func TestDryRunTextRefusalLeadsWithTheFirstRefusedOperation(t *testing.T) {
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "app", "users", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         "ALTER TABLE app.users DROP COLUMN ref, DROP COLUMN email",
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{
			{Operation: "DROP COLUMN ref", Destructive: true, Route: planner.RouteRefuse, Reason: planner.ReasonReplicaIdentity},
			{
				Operation:      "DROP COLUMN email",
				Destructive:    true,
				Route:          planner.RouteRefuse,
				Reason:         planner.ReasonDependentView,
				DependentViews: []string{`"app"."active_users"`},
			},
		},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := strings.Join(strings.Fields(out.String()), " ")
	assert.Contains(t, text, "error[unsupported-statement]: refused — the planner knows no safe path for DROP COLUMN ref")
	assert.NotContains(t, text, "refused — DROP COLUMN email changes a column read by")
	assert.Contains(t, text, "DROP COLUMN email — changes a column a view reads")
	assert.Contains(t, text, "help: drop or redefine those views first, then re-plan")
}

// A refused generated-expression change says why it cannot run, rather
// than the generic no-safe-path sentence, so the reader knows the drift is
// real and what applying it would cost.
func TestDryRunTextGeneratedExpressionRefusal(t *testing.T) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table, report.ServerVersion = "public", "events", "17.6"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "public"."events" ALTER COLUMN "name_upper" SET EXPRESSION AS (lower((name)::text))`,
		Kind:        schemadiff.ChangeSetGeneratedExpression,
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{{
			Operation: "unrecognized operation",
			Route:     planner.RouteRefuse,
			Reason:    planner.ReasonUnsupportedOperation,
		}},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := out.String()
	assert.Contains(t, text, "error[unsupported-statement]:\n  refused — changing a stored generated column's expression recomputes\n  every row under an exclusive lock")
	assert.Contains(t, text, "no online path for it, so this change is shown for review only. SET\n  EXPRESSION needs PostgreSQL 17 or later; before that, the only path is\n  to drop and re-add the column, which also drops its indexes\n")
	assert.NotContains(t, text, "the planner knows no safe path")
}

// A refusal that names an owner renders class and owner on one note, so
// the human reading the text sees who the work is handed to; a statement
// the report leaves unclassified renders no class note at all rather than
// a blank one.
func TestDryRunTextRefusalClassAndOwner(t *testing.T) {
	statement := func(class verdict.Class, owner verdict.Owner) plan.Report {
		report := plan.NewReport(plan.SourceAlter)
		report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
		report.Disposition = router.DispositionRefuse
		report.Statements = append(report.Statements, plan.Statement{
			SQL:         `GRANT SELECT ON "users" TO reporting`,
			Route:       planner.RouteRefuse,
			Disposition: router.DispositionRefuse,
			Reason:      verdict.ReasonUnsupportedStatement,
			Class:       class,
			Owner:       owner,
		})
		return report
	}

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, statement(verdict.ClassNoOnlineSafetyProblem, verdict.OwnerProvisioning)))
	assert.Contains(t, out.String(), "note:\n  refusal class: no-online-safety-problem; owner: provisioning\n")

	out.Reset()
	require.NoError(t, writeDryRunText(&out, palette{}, statement("", "")))
	assert.NotContains(t, out.String(), "refusal class")
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
	require.NoError(t, writeDryRunText(&out, palette{}, report))
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
	require.NoError(t, writeDryRunText(&out, palette{}, report))
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
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	assert.Contains(t, out.String(), "public.users (PostgreSQL 16.14) —")
	assert.NotContains(t, out.String(), "Debian")
}

// A refused foreign-key drop says in words why it is not applied
// declaratively and names the one remedy its cause calls for, so the reader
// of the diff knows what to change. A report with no cause, such as one
// decoded from JSON, names every remedy.
func TestDryRunTextDeclarativeForeignKeyRefusalDisclosesUnsupported(t *testing.T) {
	const (
		changed      = "desired-state apply does not drop this foreign key: the file changes it under the same name on a partitioned table, which would leave writes checked against no key until the new one is added, so declare the changed key under a new name"
		columnFirst  = "desired-state apply does not drop this foreign key: the plan also retypes a column, or drops one beside an added column that may rewrite the table, so it drops the key before the new one is added, which would leave writes checked against no key in between, so apply the column change on its own first, then the foreign key change"
		undeclarable = "desired-state apply does not drop this foreign key: a desired file cannot declare it, since it references another schema or its own table through a standalone unique index, so to remove it, run the statement with pg-sprite migrate --alter"
	)
	for _, tt := range []struct {
		name  string
		cause verdict.Cause
		want  string
	}{
		{"a key changed under the same name", verdict.CauseChangedForeignKey, changed},
		{"a key a column change drops first", verdict.CauseColumnFirstForeignKey, columnFirst},
		{"a key a desired file cannot declare", verdict.CauseUndeclarableForeignKey, undeclarable},
		{"a refusal with no cause", "", "desired-state apply does not drop this foreign key: it would leave writes checked against no key until the new one is added. " +
			"If the file changes it under the same name on a partitioned table, declare the changed key under a new name; " +
			"if the plan also retypes a column, or drops one beside an added column that may rewrite the table, apply the column change on its own first, then the foreign key change; " +
			"if a desired file cannot declare it, since it references another schema or its own table through a standalone unique index, run the statement with pg-sprite migrate --alter to remove it"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := plan.NewReport(plan.SourceDiff)
			report.Schema, report.Table, report.ServerVersion = "public", "orders", "16.10"
			report.Disposition = router.DispositionRefuse
			report.Statements = append(report.Statements, plan.Statement{
				SQL:          "ALTER TABLE public.orders DROP CONSTRAINT orders_user_id_fkey",
				Kind:         schemadiff.ChangeDropForeignKey,
				Route:        planner.RouteNative,
				Disposition:  router.DispositionRefuse,
				Reason:       verdict.ReasonDeclarativeForeignKey,
				Class:        verdict.ClassCapabilityBoundary,
				RefusalCause: tt.cause,
			})

			var out strings.Builder
			require.NoError(t, writeDryRunText(&out, palette{}, report))
			// The renderer wraps prose; compare with whitespace collapsed.
			flat := strings.Join(strings.Fields(out.String()), " ")
			assert.Contains(t, flat, "error[declarative-foreign-key]: refused — "+tt.want)
			if tt.cause != "" {
				assert.NotContains(t, flat, "either the file", "only the remedy the cause calls for is named")
			}
		})
	}
}

// A column add refused at the column limit says why in the refusal
// sentence and in each add's own line, naming the partition that has used
// every number, and every such add of a statement shares one help line with
// that partition's rebuild.
func TestDryRunTextColumnLimitRefusal(t *testing.T) {
	numbers := &preflight.ColumnNumbers{Fullest: []preflight.FullTable{{Name: `"app"."events_1"`, Partition: true}}}
	report := plan.NewReport(plan.SourceAlter)
	report.Schema, report.Table, report.ServerVersion = "app", "events", "16.10"
	report.Disposition = router.DispositionRefuse
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         "ALTER TABLE app.events ADD COLUMN kind text, ADD COLUMN note text",
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Class:       verdict.ClassCapabilityBoundary,
		Decisions: []planner.Decision{
			{Operation: "ADD COLUMN kind", Route: planner.RouteRefuse, Reason: planner.ReasonColumnLimit, ColumnLimit: numbers},
			{Operation: "ADD COLUMN note", Route: planner.RouteRefuse, Reason: planner.ReasonColumnLimit, ColumnLimit: numbers},
		},
	})

	var out strings.Builder
	require.NoError(t, writeDryRunText(&out, palette{}, report))
	text := strings.Join(strings.Fields(out.String()), " ")
	assert.Contains(t, text, `error[unsupported-statement]: refused — ADD COLUMN kind adds a column to a table whose partition "app"."events_1" has used all 1600 of PostgreSQL's column numbers, and a dropped column keeps its number until the table is rebuilt`)
	assert.Contains(t, text, `note[column-limit]: ADD COLUMN note — adds a column to a table whose partition "app"."events_1" has used all 1600 of PostgreSQL's column numbers; a dropped column keeps its number until the table is rebuilt`)
	assert.Equal(t, 1, strings.Count(text, "help: "+planner.ColumnLimitHelp(numbers)))
	assert.Contains(t, text, `help: copy partition "app"."events_1" into a new table`)
	assert.Contains(t, text, onlineDDLReferenceURL+"#column-limit")
	assert.NotContains(t, text, "knows no safe path")
}
