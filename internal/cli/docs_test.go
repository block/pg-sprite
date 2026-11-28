package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/lint"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
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
	require.Len(t, blocks, 22, "the doc publishes twenty-two captured --json outputs")

	// A live plan proves the added column against the server; a nullable
	// column with no default is added in place, so the plan marks it
	// catalog-only.
	noteFacts := usersFacts()
	noteFacts.AddColumnProofs = map[string]statement.AddColumnProof{"note": statement.AddColumnInPlace}
	metadataOnly := alterReport(t, "ALTER TABLE users ADD COLUMN note text",
		"public", "users", noteFacts)
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
	acceptedBlocking, err := (verdict.Verdict{
		Statement:  "DROP INDEX public.users_email_idx",
		Table:      "public.users",
		SaferIdiom: "DROP INDEX CONCURRENTLY",
	}).WithAcceptedBlocking(
		verdict.ByDesign(verdict.ReasonIndexStatement).WithSite(verdict.RefusalSiteIndexSingleRelation),
		3*time.Second, 10*time.Minute)
	require.NoError(t, err)
	acceptedBlocking.AllowedDestructive = true
	rewriteRequired := alterReport(t, "ALTER TABLE users ADD COLUMN nickname text UNIQUE",
		"public", "users", usersFacts())
	copyAndSwapRouted := alterReport(t, "ALTER TABLE users ALTER COLUMN id TYPE text",
		"public", "users", usersFacts())
	// The failed verdict is captured from a run: a CHECK whose NOT VALID
	// step commits and whose VALIDATE step then meets a violating row. Its
	// detail is the migrate package's rendering of a mid-sequence failure.
	failedSequence := verdict.Verdict{
		Outcome:       verdict.OutcomeFailed,
		Code:          string(executor.CodeExecutionFailed),
		SQLState:      "23514",
		ServerError:   `check constraint "users_email_lower" of relation "users" is violated by some row`,
		FailedStep:    2,
		FailedStepSQL: `ALTER TABLE "public"."users" VALIDATE CONSTRAINT "users_email_lower"`,
		Statement:     "ALTER TABLE public.users ADD CONSTRAINT users_email_lower CHECK (email = lower(email))",
		Table:         "public.users",
		Detail: "sequence step 2 of 2 failed; the 1 committed steps' state remains — " +
			"the planner sequence's partial-failure contract says how a retry resumes",
		ExecutedSQL: []string{
			"ALTER TABLE public.users ADD CONSTRAINT users_email_lower CHECK (email = lower(email)) NOT VALID",
		},
	}
	// The data-fault verdict is captured from the demo tour's narrowing of
	// orders.note: the seeded long note fails the chunk's insert, and the
	// detail is the migrate package's rendering of the abandoned run.
	dataFault := verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.CodeDataFault),
		Statement: "ALTER TABLE public.orders ALTER COLUMN note TYPE varchar(32)",
		Table:     "public.orders",
		Detail: "the copy-and-swap run stopped: run public.orders: copy-and-swap run on public.orders abandoned on a data fault: " +
			"copy chunk [2001, 4000] of public.orders into _pgsprite_c74da4a21f58f903_new: " +
			"ERROR: value too long for type character varying(32) (SQLSTATE 22001); " +
			"the server rejected the rows as the statement would shape them, so the run was abandoned: " +
			"the shadow is dropped, its replication slot is dropped, and the checkpoint row is deleted; " +
			"correct the data or the statement, then run it again, which starts from the beginning",
		Abandoned: true,
	}
	// The mode-mismatch verdict is captured from a run: a --block-writes
	// retype of orders stopped mid-copy, then run again without the flag.
	// Its detail is the migrate package's rendering of the refusal, which
	// TestBlockWritesRunStoppedMidCopyNamesTheWayOut pins.
	modeMismatch := verdict.Verdict{
		Statement: "ALTER TABLE public.orders ALTER COLUMN qty TYPE bigint",
		Table:     "public.orders",
		Detail: "a copy-and-swap run of this statement on public.orders stopped with the table's writes blocked, " +
			"and this run would stream writes through a replication slot instead, so it cannot continue that run; " +
			"finish the stopped run by running the same command again with --block-writes public.orders added (CopySwapOptions.BlockWrites), " +
			"or abandon it with pg-sprite discard public.orders (schemachange.Runner.Discard); " +
			"until then the source stays fenced and every write to it fails with SQLSTATE 55006; nothing was executed",
		WritesBlocked: true,
	}.WithRefusal(verdict.Environmental(verdict.ReasonCopyAndSwapRefused).WithCause("resume-mode-mismatch"))
	// The executed verdict is captured from a run; its detail is the
	// verdict's own rendering of the verification pass and the retained
	// source's fate, which the migrate package's tests pin.
	executedViaCopyAndSwap := verdict.Verdict{
		Outcome:   verdict.OutcomeExecutedViaCopyAndSwap,
		Statement: "ALTER TABLE users ALTER COLUMN id TYPE text",
		Table:     "public.users",
		Detail:    "verified 1000 rows in 2 chunks; the retained source is dropped",
		SwapLSN:   "0/1A2B3C8",
		Verification: &verdict.Verification{
			Rows:   1000,
			Chunks: 2,
		},
	}
	// A live plan proves the default against the server; clock_timestamp()
	// is computed per row, so the probe reports the default rewrites.
	volatileDefaultFacts := usersFacts()
	volatileDefaultFacts.AddColumnProofs = map[string]statement.AddColumnProof{"joined": statement.AddColumnDefaultRewrites}
	backendUnavailable := alterReport(t, "ALTER TABLE users ADD COLUMN joined timestamptz DEFAULT clock_timestamp()",
		"public", "users", volatileDefaultFacts)

	// A CREATE INDEX on the partitioned events table is routed to the
	// partitioned index build; the executed verdict is captured from a run,
	// and its detail is the migrate package's rendering of the build.
	partitionedBuild := alterReport(t, "CREATE INDEX events_created_idx ON events (created)",
		"public", "events", eventsFacts())
	require.NoError(t, plan.RoutePartitionedParent(&partitionedBuild, 16))
	partitionedBuild.Fingerprint = plan.Fingerprint(partitionedBuild.Statements)
	partitionedBuilt := verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: "CREATE INDEX events_created_idx ON public.events USING btree (created)",
		Table:     "public.events",
		Detail: "the table is partitioned, so pg-sprite built index events_created_idx partition by partition " +
			"and attached each one: 2 partitions, 2 given an index by this run; the index is valid",
	}

	// Adopting an index as a constraint stays refused on a partitioned parent.
	partitioned := alterReport(t, "ALTER TABLE events ADD CONSTRAINT events_pkey PRIMARY KEY USING INDEX events_id_idx",
		"public", "events", eventsFacts())
	require.NoError(t, plan.RoutePartitionedParent(&partitioned, 16))
	partitioned.Fingerprint = plan.Fingerprint(partitioned.Statements)

	destructive := alterReport(t, "ALTER TABLE users DROP COLUMN email",
		"public", "users", usersFacts())
	// Without the acknowledgement the dry run ends in the apply's refusal
	// verdict instead of the plan report above.
	dropEmail, err := statement.ParseOne("ALTER TABLE users DROP COLUMN email")
	require.NoError(t, err)
	dropEmailCanonical, err := statement.Canonical(dropEmail.SQL())
	require.NoError(t, err)
	dropEmailClassified, err := planner.Classify(dropEmailCanonical, usersFacts())
	require.NoError(t, err)
	destructiveRefused, refused, err := migrate.AdmitDestructive(dropEmail, "public",
		router.Route([]planner.Plan{dropEmailClassified}).Statements[0], "")
	require.NoError(t, err)
	require.True(t, refused)

	// A desired file that no longer has the note column: the dry run ends in
	// the refused plan, whose fingerprint cannot be pinned, and carries the
	// one a run pins.
	ordersFacts := planner.Facts{ColumnTypes: map[string]string{"id": "bigint", "user_id": "bigint", "note": "text"}}
	dropNote := alterReport(t, "ALTER TABLE public.orders DROP note", "public", "orders", ordersFacts)
	dropNote.Source = plan.SourceDiff
	for i := range dropNote.Statements {
		dropNote.Statements[i].Kind = schemadiff.ChangeDropColumn
		dropNote.Statements[i].Drops = &schemadiff.DroppedObject{
			Kind: schemadiff.DroppedColumn, Name: "note", Definition: "attnum 3: text",
		}
	}
	dropNote.Fingerprint = plan.Fingerprint(dropNote.Statements)
	plan.RefuseDestructive(&dropNote)

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

	_, typeFileErr := statement.ParseDesiredFile("CREATE DOMAIN account_email AS text;\n")
	typeFileRefused, ok := migrate.DesiredFileRefusal(typeFileErr, "")
	require.True(t, ok, "a domain file is a desired-file refusal: %v", typeFileErr)

	var lintDesiredOut strings.Builder
	lintDesiredErr := (&LintCmd{Desired: true, JSON: true}).runLint(strings.NewReader(
		"CREATE TABLE events (id bigint NOT NULL, at date NOT NULL) PARTITION BY RANGE (at);\n"+
			"CREATE TABLE events_2026 PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');\n"), &lintDesiredOut)
	require.ErrorIs(t, lintDesiredErr, verdict.ErrRefused)
	var partitionsRefused verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(lintDesiredOut.String()), &partitionsRefused))

	accounts, err := statement.ParseDesiredFile("CREATE TABLE accounts (\n  id bigint PRIMARY KEY,\n  status text NOT NULL\n);\n")
	require.NoError(t, err)
	_, tunedErr := schemadiff.Diff("app",
		schemadiff.Model{Table: "accounts", Options: []string{"fillfactor=70"}},
		schemadiff.Model{Table: "accounts"})
	tunedRefused, ok := migrate.DesiredFileRefusal(tunedErr, accounts.Statements()[0].SQL())
	require.True(t, ok, "a storage parameter difference is a desired-file refusal: %v", tunedErr)

	want := []any{metadataOnly, saferIdiom, executed, acceptedBlocking, failedSequence, dataFault, modeMismatch, rewriteRequired,
		copyAndSwapRouted, executedViaCopyAndSwap, backendUnavailable, partitionedBuild, partitionedBuilt, partitioned,
		destructiveRefused, destructive, dropNote, lintReport, partitionsRefused, diff, typeFileRefused, tunedRefused}
	for i, w := range want {
		marshaled, err := json.Marshal(w)
		require.NoError(t, err)
		assert.JSONEq(t, blocks[i][1], string(marshaled),
			"docs/cli-output-examples.md example %d drifted from pipeline output", i+1)
	}
}

// The capabilities example is the one --json block the doc truncates with
// `| head`, so it is not a complete JSON document and the check above cannot
// see it. It is still captured output: the published lines must be exactly
// what the command prints first, byte for byte, for a "dev" build.
func TestCLIOutputExamplesCapabilitiesHeadMatchesCommand(t *testing.T) {
	raw, err := os.ReadFile(cliOutputExamplesDoc)
	require.NoError(t, err)
	blocks := regexp.MustCompile("(?s)```console\n\\$ pg-sprite capabilities --json \\| head\n(.*?)```").FindAllStringSubmatch(string(raw), -1)
	require.Len(t, blocks, 1, "the doc publishes one truncated capabilities --json output")
	published := strings.Split(strings.TrimSuffix(blocks[0][1], "\n"), "\n")

	cmd := CapabilitiesCmd{JSON: true, version: "dev"}
	var out bytes.Buffer
	require.NoError(t, cmd.run(&out))
	printed := strings.Split(out.String(), "\n")
	require.Greater(t, len(printed), len(published))

	assert.Equal(t, printed[:len(published)], published,
		"docs/cli-output-examples.md capabilities example drifted from command output")
}
