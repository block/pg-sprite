package cli

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

// dryRunPlan runs migrate --dry-run --json and decodes the plan report. The
// full report is written either way; the helper enforces the dry-run exit
// contract alongside it: an executable plan returns nil, a plan execution
// would not run returns ErrRefused so the process exits with the refusal
// code.
func dryRunPlan(t *testing.T, url, alter string) plan.Report {
	t.Helper()
	return dryRunPlanOf(t, newMigrateCmd(url, alter))
}

// dryRunPlanOf is dryRunPlan for a command the caller configured, such as
// one carrying --allow-destructive.
func dryRunPlanOf(t *testing.T, cmd *MigrateCmd) plan.Report {
	t.Helper()
	cmd.DryRun = true
	cmd.JSON = true
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	require.Equal(t, plan.FormatVersion, report.FormatVersion)
	require.Equal(t, plan.SourceAlter, report.Source)
	missingTable := report.TableExists != nil && !*report.TableExists
	if report.Disposition == router.DispositionExecute && !missingTable {
		require.NoError(t, err)
	} else {
		require.ErrorIs(t, err, verdict.ErrRefused)
	}
	return report
}

// An index rename names no table, so its dry run reports the table that
// owns the index and plans from the live fact of whether the index backs a
// constraint: a plain index is executable, and the index behind a unique
// constraint is rewrite-required with the coordination guidance, exiting
// with the refusal code. Nothing is renamed either way.
func TestMigrateDryRunPlansAnIndexRenameFromTheConstraintItBacks(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (
			id     int PRIMARY KEY,
			email  text NOT NULL,
			handle text NOT NULL,
			CONSTRAINT users_handle_key UNIQUE (handle)
		);
		CREATE INDEX users_email_idx ON %[1]s.users (email)`, schema))
	require.NoError(t, err)

	plain := dryRunPlan(t, url, fmt.Sprintf("ALTER INDEX %s.users_email_idx RENAME TO users_contact_email_idx", schema))
	assert.Equal(t, router.DispositionExecute, plain.Disposition)
	assert.Equal(t, schema, plain.Schema)
	assert.Equal(t, "users", plain.Table, "the report names the index's owning table")
	require.Len(t, plain.Statements, 1)
	assert.Equal(t, planner.ReasonMetadataOnly, plain.Statements[0].Decisions[0].Reason)

	backing := dryRunPlan(t, url, fmt.Sprintf("ALTER INDEX %s.users_handle_key RENAME TO users_username_key", schema))
	assert.Equal(t, router.DispositionRewriteRequired, backing.Disposition)
	assert.Equal(t, "users", backing.Table)
	require.Len(t, backing.Statements, 1)
	assert.Equal(t, planner.ReasonAppBreakingRename, backing.Statements[0].Decisions[0].Reason)
	assert.Equal(t, suggest.GuidanceCoordinatedConstraintRename, backing.Statements[0].Guidance)

	var names int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_class WHERE relnamespace = $1::regnamespace AND relname IN ('users_email_idx', 'users_handle_key')`,
		schema).Scan(&names))
	assert.Equal(t, 2, names, "dry-run must not rename anything")
}

// A rewrite-requiring type change dry-runs to the copy-and-swap backend as
// executable — the route the real run takes — and nothing executes: the
// live column type is untouched.
func TestMigrateDryRunRoutesRewriteWithoutExecuting(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	report := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, planner.RouteCopyAndSwap, st.Route)
	assert.Equal(t, router.BackendCopyAndSwap, st.Backend)
	assert.Empty(t, st.ExecSQL, "the copy-and-swap backend lists no literal SQL")

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "integer", typ, "dry-run must not execute the change")
}

// A plain CREATE INDEX on a partitioned parent plans as the partitioned
// index build: one executable step that is the submitted statement, with no
// CONCURRENTLY form for the reader to mistake for what runs.
func TestMigrateDryRunRoutesPartitionedParentIndexPlan(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := createPartitionFixture(t, pool)
	sql := fmt.Sprintf("CREATE INDEX p_v_idx ON %s.p (v)", schema)
	report := dryRunPlan(t, url, sql)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, planner.ExecutionPartitionedIndexBuild, st.Execution)
	require.Len(t, st.ExecSQL, 1)
	assert.NotContains(t, st.ExecSQL[0], "CONCURRENTLY")
	require.NotEmpty(t, st.Decisions)
	for _, decision := range st.Decisions {
		assert.Equal(t, st.ExecSQL, decision.SaferSQL)
		assert.Equal(t, planner.ExecutionPartitionedIndexBuild, decision.SaferSQLExecution)
	}
}

// Live facts feed the imperative dry-run: a widen the classifier can only
// prove with the live column type routes native, and still executes nothing.
func TestMigrateDryRunUsesLiveFacts(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, name varchar(20))", schema))
	require.NoError(t, err)

	alter := fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN name TYPE varchar(50)", schema)
	report := dryRunPlan(t, url, alter)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, planner.RouteNative, st.Route)
	assert.Equal(t, router.BackendNative, st.Backend)
	require.Len(t, st.Decisions, 1)
	assert.Equal(t, planner.ReasonBinaryCoercible, st.Decisions[0].Reason,
		"the live varchar(20) must be introspected to prove the widen")
	assert.Equal(t, []string{alter}, st.ExecSQL)

	var maxLen int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT character_maximum_length FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'name'`, schema).Scan(&maxLen))
	assert.Equal(t, 20, maxLen, "dry-run must not execute the change")
}

// timestamptz(3) -> timestamp(3) rewrites the table under ACCESS EXCLUSIVE,
// so the dry-run must route it to copy-and-swap and never call it a
// relabel. The live type is the real server's format_type rendering
// ("timestamp(3) with time zone"), whose text after the modifiers is the only
// thing that tells it apart from timestamp(3).
func TestMigrateDryRunRoutesTimestamptzToTimestampAsRewrite(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.t (
		id int PRIMARY KEY,
		ts timestamptz(3)
	)`, schema))
	require.NoError(t, err)

	report := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN ts TYPE timestamp(3)", schema))
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, planner.RouteCopyAndSwap, st.Route)
	assert.Equal(t, router.BackendCopyAndSwap, st.Backend)
	assert.Empty(t, st.ExecSQL)
	require.Len(t, st.Decisions, 1)
	assert.Equal(t, planner.ReasonTypeRewrite, st.Decisions[0].Reason)
}

// A column with an explicit collation that is retyped without a COLLATE
// clause takes the type's default collation, so every index on it is rebuilt
// under ACCESS EXCLUSIVE and its sort order changes. That is not a relabel:
// the dry-run must read the live collation and route to copy-and-swap even
// though the type part is a plain varchar widen.
func TestMigrateDryRunRoutesCollatedColumnRetypeAsRewrite(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.t (
		id int PRIMARY KEY,
		c varchar(50) COLLATE "C"
	)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE INDEX t_c ON %s.t (c)`, schema))
	require.NoError(t, err)

	report := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN c TYPE varchar(100)", schema))
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, planner.RouteCopyAndSwap, st.Route)
	assert.Empty(t, st.ExecSQL, "dry-run must not offer the native statement")
	require.Len(t, st.Decisions, 1)
	assert.Equal(t, planner.ReasonTypeRewrite, st.Decisions[0].Reason)
	assert.False(t, st.Decisions[0].Unverified)
}

// The dry-run advisory covers statements the execute gate refuses: a plain
// CREATE INDEX comes back native with its concurrent rewrite, not created.
func TestMigrateDryRunSuggestsConcurrentIndex(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	submitted := fmt.Sprintf("CREATE INDEX t_id_idx ON %s.t (id)", schema)
	report := dryRunPlan(t, url, submitted)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, planner.RouteNative, st.Route)
	require.Len(t, st.Decisions, 1)
	assert.Equal(t, planner.ReasonSaferIdiom, st.Decisions[0].Reason)
	require.Len(t, st.ExecSQL, 1)
	assert.NotEqual(t, submitted, st.ExecSQL[0], "the plan carries the concurrent rewrite")

	var indexes int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 't_id_idx'`,
		schema).Scan(&indexes))
	assert.Equal(t, 0, indexes, "dry-run must not create the index")
}

// A safer-idiom decision without a constructed rewrite dry-runs to
// rewrite-required with no executable SQL, and nothing executes: the
// engine must not fall back to the submitted blocking form.
func TestMigrateDryRunInlineConstraintIsRewriteRequired(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	plan := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN c int UNIQUE", schema))
	assert.Equal(t, router.DispositionRewriteRequired, plan.Disposition)
	require.Len(t, plan.Statements, 1)
	st := plan.Statements[0]
	assert.Equal(t, planner.RouteNative, st.Route)
	assert.Equal(t, router.BackendNative, st.Backend)
	require.Len(t, st.Decisions, 1)
	assert.Equal(t, planner.ReasonSaferIdiom, st.Decisions[0].Reason)
	assert.Empty(t, st.ExecSQL, "no executable SQL for an unconstructed rewrite")

	var columns int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'c'`, schema).Scan(&columns))
	assert.Equal(t, 0, columns, "dry-run must not add the column")
}

// Both front doors must agree on destructiveness: the same DROP COLUMN is
// destructive whether a human submits it directly (the riskier door) or the
// diff derives it from a reviewed desired-state file. Destructive is derived
// from the classifier's decisions, so the agreement holds by construction.
// The imperative dry run reports the plan only under --allow-destructive;
// without it the dry run ends in the apply's destructive-change refusal.
func TestDryRunAndDiffAgreeOnDestructive(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, doomed text)", schema))
	require.NoError(t, err)

	alter := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN doomed", schema))
	alter.AllowDestructive = schema + ".t"
	alterReport := dryRunPlanOf(t, alter)
	require.Len(t, alterReport.Statements, 1)
	alterSt := alterReport.Statements[0]
	assert.True(t, alterSt.Destructive, "the submitted DROP COLUMN must be marked destructive")

	cmd := newDiffCmd(t, url, schema, "CREATE TABLE t (id int PRIMARY KEY);")
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var diffReport plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &diffReport))
	require.Len(t, diffReport.Statements, 1)
	diffSt := diffReport.Statements[0]

	assert.Equal(t, alterSt.Destructive, diffSt.Destructive, "front doors must agree on destructive")
	assert.Equal(t, alterSt.Route, diffSt.Route)
	assert.Equal(t, alterSt.Backend, diffSt.Backend)
	assert.Equal(t, alterSt.Disposition, diffSt.Disposition)
	assert.Equal(t, alterSt.SQL, diffSt.SQL,
		"both doors must render the same change as the same canonical string")
	assert.NotEmpty(t, alterReport.Fingerprint)

	// The diff door also reports the live column the drop removes, which its
	// fingerprint covers and a submitted statement has no way to carry.
	// Without that definition the two doors name the same plan.
	require.NotNil(t, diffSt.Drops, "the derived DROP COLUMN must carry the column it removes")
	assert.Equal(t, schemadiff.DroppedColumn, diffSt.Drops.Kind)
	assert.Equal(t, "doomed", diffSt.Drops.Name)
	assert.NotEqual(t, alterReport.Fingerprint, diffReport.Fingerprint,
		"a pin on the derived plan must bind the definition of the column it drops")
	withoutDefinition := slices.Clone(diffReport.Statements)
	withoutDefinition[0].Drops = nil
	assert.Equal(t, alterReport.Fingerprint, plan.Fingerprint(withoutDefinition),
		"the same plan must carry the same identity through either door")
}

// An unqualified statement resolves to the schema the engine actually
// introspected (public, on the default search_path) and the report says
// so: a stored plan must not depend on the reader's search_path to name
// its target. The report also
// stamps the server version its classification was derived against.
func TestDryRunResolvesUnqualifiedSchemaAndStampsServerVersion(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	// A uniquely named table in public: unqualified statements resolve
	// there, and the unique name keeps a shared PG_DSN database safe.
	table := testutil.NewPublicTable(t, pool, "(id int PRIMARY KEY, v varchar(50))")

	report := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN v TYPE varchar(100)", table))
	assert.Equal(t, "public", report.Schema,
		"the report must name the schema the engine introspected, not echo the submitted qualification")
	assert.NotEmpty(t, report.ServerVersion,
		"the report must stamp the server version its classification came from")
	require.NotNil(t, report.TableExists,
		"a dry run that introspected the target must say so")
	assert.True(t, *report.TableExists)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.ReasonBinaryCoercible, report.Statements[0].Decisions[0].Reason,
		"resolving to public must feed the live facts to the classifier")
}

// A dry-run reads an unqualified table where the session search_path
// resolves it, as execution does, even when public holds a table of the
// same name. The schema it resolves to already holds the server's first
// choice for an unnamed CHECK, so the plan names the constraint as the
// server would there, and execution runs exactly the planned SQL.
func TestMigrateDryRunResolvesTheTableThroughSearchPath(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	table := testutil.NewPublicTable(t, pool, "(qty int)")
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.%s (qty int, CONSTRAINT %s_qty_check CHECK (qty < 1000))", schema, table, table))
	require.NoError(t, err)

	u, err := neturl.Parse(url)
	require.NoError(t, err)
	q := u.Query()
	q.Set("options", "-csearch_path="+schema+",public")
	u.RawQuery = q.Encode()
	alter := fmt.Sprintf("ALTER TABLE %s ADD CHECK (qty > 0)", table)

	report := dryRunPlan(t, u.String(), alter)
	assert.Equal(t, schema, report.Schema, "the plan names the schema the search_path resolves the table to")
	require.Len(t, report.Statements, 1)
	planned := report.Statements[0].ExecSQL
	require.Len(t, planned, 2)
	assert.Contains(t, planned[0], table+"_qty_check1", "the plan numbers past the name the resolved schema holds")

	cmd := newMigrateCmd(u.String(), alter)
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	require.Len(t, v.ExecutedSQL, 2)
	assert.Contains(t, v.ExecutedSQL[0], table+"_qty_check1", "execution names the constraint as the plan did")
}

// A dry-run against a table that does not exist classifies with zero facts:
// the unprovable type change routes conservatively to copy-and-swap instead
// of failing, marked unverified, and the report records the missing target.
func TestMigrateDryRunMissingTableIsConservative(t *testing.T) {
	url := testutil.StartPostgres(t)

	report := dryRunPlan(t, url, "ALTER TABLE missing ALTER COLUMN v TYPE varchar(50)")
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, report.Statements[0].Route)
	assert.True(t, report.Statements[0].Decisions[0].Unverified)
	require.NotNil(t, report.TableExists)
	assert.False(t, *report.TableExists)
}

// A statement that classifies as executable against a missing table must
// not exit green: applying it would fail, so the dry run carries the
// refusal exit code and marks the missing target in the report.
func TestMigrateDryRunMissingTableRefusesExecutablePlan(t *testing.T) {
	url := testutil.StartPostgres(t)

	cmd := newMigrateCmd(url, "ALTER TABLE nosuchtable ADD COLUMN z int")
	cmd.DryRun = true
	cmd.JSON = true
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused,
		"a plan classified from zero facts must not gate green")

	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Equal(t, router.DispositionExecute, report.Disposition,
		"the routed disposition itself is executable — the refusal exit comes from the missing target")
	require.NotNil(t, report.TableExists)
	assert.False(t, *report.TableExists)
}

// admissionRefusalVerdicts runs alter as a dry run and as an apply, requires
// both to end in the refusal exit, and returns the two refusal verdicts. The
// dry run goes first; neither run changes the database, because the apply
// refuses before executing anything.
func admissionRefusalVerdicts(t *testing.T, url, alter string) (dryRun, applied verdict.Verdict) {
	t.Helper()
	run := func(dry bool) verdict.Verdict {
		cmd := newMigrateCmd(url, alter)
		cmd.DryRun = dry
		cmd.JSON = true
		var out strings.Builder
		require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused,
			"the refusal exit code must not depend on --dry-run")
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		return v
	}
	return run(true), run(false)
}

// PostgreSQL refuses DETACH PARTITION CONCURRENTLY — the form the planner
// substitutes for a plain DETACH PARTITION — on a table with a DEFAULT
// partition, so the apply refuses it from the catalog before it runs
// anything. The dry run must end in that same refusal instead of promising
// an executable plan the apply would turn down.
func TestMigrateDryRunRefusesDetachBesideADefaultPartitionLikeApply(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := createPartitionFixture(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %[1]s.p_default PARTITION OF %[1]s.p DEFAULT", schema))
	require.NoError(t, err)

	run := func(dry bool) verdict.Verdict {
		cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %[1]s.p DETACH PARTITION %[1]s.p1", schema))
		cmd.DryRun = dry
		cmd.JSON = true
		cmd.AllowDestructive = schema + ".p"
		var out strings.Builder
		require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		return v
	}
	dryRun, applied := run(true), run(false)

	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, dryRun.Reason)
	assert.Equal(t, verdict.ClassByDesign, dryRun.Class)
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the apply reports")
}

// Without a DEFAULT partition, the dry run of a plain DETACH PARTITION plans
// the concurrent detach the apply then runs.
func TestMigrateDryRunPlansTheConcurrentDetachTheApplyRuns(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := createPartitionFixture(t, pool)
	sql := fmt.Sprintf("ALTER TABLE %[1]s.p DETACH PARTITION %[1]s.p1", schema)

	cmd := newMigrateCmd(url, sql)
	cmd.DryRun = true
	cmd.JSON = true
	cmd.AllowDestructive = schema + ".p"
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	require.Len(t, report.Statements, 1)
	assert.Equal(t, router.DispositionExecute, report.Statements[0].Disposition)
	assert.True(t, report.Statements[0].Destructive)
	assert.Equal(t, []string{sql + " CONCURRENTLY"}, report.Statements[0].ExecSQL)

	cmd.DryRun = false
	out.Reset()
	require.NoError(t, cmd.run(t.Context(), &out))
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, report.Statements[0].ExecSQL, v.ExecutedSQL)
}

// The concurrent index build executor needs a name for the index it builds
// and later verifies, so an unnamed CREATE INDEX is refused before anything
// executes. The dry run must report that refusal, not the plan that would
// have run into it.
func TestMigrateDryRunRefusesUnnamedCreateIndexLikeApply(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	dryRun, applied := admissionRefusalVerdicts(t, url, fmt.Sprintf("CREATE INDEX ON %s.t (c)", schema))

	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, dryRun.Reason)
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the apply reports")
}

// IF NOT EXISTS is refused on an index build — the executor cannot tell a
// skipped build from a finished one — and that refusal is a permanent design
// decision, so its class differs from the capability boundary the other two
// admission refusals carry. The dry run must carry the apply's class too.
func TestMigrateDryRunRefusesIfNotExistsCreateIndexLikeApply(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	dryRun, applied := admissionRefusalVerdicts(t, url,
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS t_c_idx ON %s.t (c)", schema))

	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, dryRun.Reason)
	assert.Equal(t, verdict.ClassByDesign, dryRun.Class)
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the apply reports")
}

// A named, schema-qualified CREATE INDEX passes admission, so the dry run
// keeps reporting the plan it always did: the check refuses only what the
// apply refuses.
func TestMigrateDryRunAdmitsNamedCreateIndex(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	report := dryRunPlan(t, url, fmt.Sprintf("CREATE INDEX t_c_idx ON %s.t (c)", schema))

	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Len(t, report.Statements[0].ExecSQL, 1)
}

// An unqualified table is resolved before the apply's admission runs, so the
// dry run must not read the missing schema qualification as a refusal: the
// named index build on a table in public stays an executable plan.
func TestMigrateDryRunAdmitsNamedCreateIndexOnUnqualifiedTable(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	table := testutil.NewPublicTable(t, pool, "(id int PRIMARY KEY, c int)")

	report := dryRunPlan(t, url, fmt.Sprintf("CREATE INDEX %s_c_idx ON %s (c)", table, table))

	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Len(t, report.Statements[0].ExecSQL, 1)
}

// An unqualified table resolves on the session's search_path. A table of the
// same name in public is a different relation, so a refusal that named
// public would point at the wrong table: the dry run must name the table the
// apply resolves, and carry the apply's whole verdict, statement included.
func TestMigrateDryRunAdmissionRefusalOnSearchPathTableMatchesApply(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	table := testutil.NewPublicTable(t, pool, "(id int PRIMARY KEY, c int)")
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.%s (
			id int PRIMARY KEY,
			c int
		)`, schema, table))
	require.NoError(t, err)

	u, err := neturl.Parse(url)
	require.NoError(t, err)
	q := u.Query()
	q.Set("options", "-csearch_path="+schema+",public")
	u.RawQuery = q.Encode()

	dryRun, applied := admissionRefusalVerdicts(t, u.String(), fmt.Sprintf("CREATE INDEX ON %s (c)", table))

	require.Equal(t, schema+"."+table, applied.Table)
	assert.Equal(t, applied, dryRun)
}

// A statement admission would refuse, aimed at a table that does not exist,
// still dry-runs to the missing-table report: the apply stops at the missing
// table before it reaches admission.
func TestMigrateDryRunReportsMissingTableBeforeAdmission(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	report := dryRunPlan(t, url, fmt.Sprintf("CREATE INDEX ON %s.absent (c)", schema))

	require.NotNil(t, report.TableExists)
	assert.False(t, *report.TableExists)
}

// A partitioned parent has no storage of its own, so SET TABLESPACE on it
// moves nothing: the server records the tablespace as the default for
// partitions created later, under a brief ACCESS EXCLUSIVE on the parent
// alone. The dry run plans it as a metadata-only native statement, and the
// apply runs it as written: every existing partition keeps its filenode and
// its tablespace, and a partition created afterwards lands in the new one.
func TestMigrateSetTablespaceOnAPartitionedParentMovesNoPartition(t *testing.T) {
	testutil.SkipOnSharedServer(t, "creates a tablespace directory on the server's filesystem")
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	tablespace := testutil.NewTablespace(t, pool, schema)
	exec := func(sql string) {
		t.Helper()
		_, err := pool.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}
	exec(fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint,
			created_at timestamptz,
			PRIMARY KEY (id, created_at)
		) PARTITION BY RANGE (created_at)`, schema))
	exec(fmt.Sprintf(`CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')`, schema))
	exec(fmt.Sprintf(`CREATE TABLE %[1]s.events_2025 PARTITION OF %[1]s.events FOR VALUES FROM ('2025-01-01') TO ('2026-01-01')`, schema))
	exec(fmt.Sprintf(`
		INSERT INTO %[1]s.events
		SELECT g, timestamptz '2024-01-01' + g * interval '1 day'
		FROM generate_series(1, 600) g`, schema))
	// Each partition and partition index, by name, to its filenode and its
	// tablespace (0 is the database default).
	storage := func() map[string][2]uint32 {
		t.Helper()
		rows, err := pool.Query(t.Context(), `
			SELECT c.relname, pg_catalog.pg_relation_filenode(c.oid), c.reltablespace
			FROM pg_catalog.pg_class c
			WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ('r', 'i')`, schema)
		require.NoError(t, err)
		defer rows.Close()
		out := map[string][2]uint32{}
		for rows.Next() {
			var name string
			var node, spc uint32
			require.NoError(t, rows.Scan(&name, &node, &spc))
			out[name] = [2]uint32{node, spc}
		}
		require.NoError(t, rows.Err())
		return out
	}
	before := storage()
	require.Len(t, before, 4, "two partitions and their primary key indexes")

	sql := fmt.Sprintf("ALTER TABLE %s.events SET TABLESPACE %s", schema, tablespace)
	report := dryRunPlan(t, url, sql)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, planner.RouteNative, st.Route)
	assert.Equal(t, router.BackendNative, st.Backend)
	assert.Equal(t, []string{sql}, st.ExecSQL, "the statement runs as written")
	require.Len(t, st.Decisions, 1)
	assert.Equal(t, planner.ReasonMetadataOnly, st.Decisions[0].Reason)

	cmd := newMigrateCmd(url, sql)
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)

	assert.Equal(t, before, storage(), "no partition or partition index moved or was rewritten")
	var parentTablespace, laterTablespace string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT t.spcname FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_tablespace t ON t.oid = c.reltablespace
		WHERE c.oid = $1::regclass`, schema+".events").Scan(&parentTablespace))
	assert.Equal(t, tablespace, parentTablespace, "the parent records the new default")
	exec(fmt.Sprintf(`CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`, schema))
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT t.spcname FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_tablespace t ON t.oid = c.reltablespace
		WHERE c.oid = $1::regclass`, schema+".events_2026").Scan(&laterTablespace))
	assert.Equal(t, tablespace, laterTablespace, "a partition created afterwards lands in the new tablespace")
}

// A partition has a heap of its own, so SET TABLESPACE on it is the
// rewrite-scale move, even though its parent's would be metadata-only: the
// dry run plans it as relocation on copy-and-swap, which does not carry a
// tablespace move, and the plan is unavailable. pg_default exists on every
// server, so the test needs no tablespace directory.
func TestMigrateDryRunKeepsSetTablespaceOnAPartitionOnTheRewritePath(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, sql := range []string{
		fmt.Sprintf(`
			CREATE TABLE %[1]s.events (
				id bigint,
				created_at timestamptz,
				PRIMARY KEY (id, created_at)
			) PARTITION BY RANGE (created_at)`, schema),
		fmt.Sprintf(`CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')`, schema),
	} {
		_, err := pool.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}

	report := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s.events_2024 SET TABLESPACE pg_default", schema))
	assert.Equal(t, router.DispositionUnavailable, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, report.Statements[0].Route)
	require.Len(t, report.Statements[0].Decisions, 1)
	assert.Equal(t, planner.ReasonRelocation, report.Statements[0].Decisions[0].Reason)
	assert.False(t, report.Statements[0].Decisions[0].Unverified, "the partition's storage was read live")
}

// An ordinary table has a heap, so SET TABLESPACE on it is the
// rewrite-scale move: relocation on copy-and-swap, which does not carry a
// tablespace move, so the plan is unavailable.
func TestMigrateDryRunKeepsSetTablespaceOnAnOrdinaryTableOnTheRewritePath(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.accounts (
			id bigint PRIMARY KEY
		)`, schema))
	require.NoError(t, err)

	report := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s.accounts SET TABLESPACE pg_default", schema))
	assert.Equal(t, router.DispositionUnavailable, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, report.Statements[0].Route)
	require.Len(t, report.Statements[0].Decisions, 1)
	assert.Equal(t, planner.ReasonRelocation, report.Statements[0].Decisions[0].Reason)
	assert.False(t, report.Statements[0].Decisions[0].Unverified, "the table's storage was read live")
}

// SET TABLESPACE on a partitioned parent locks the parent alone: it does
// not recurse, so a partition another session holds in EXCLUSIVE mode does
// not hold it up, and the run completes under a short lock timeout. A
// statement that took ACCESS EXCLUSIVE on the partition would wait for that
// lock and fail on the timeout. EXCLUSIVE rather than ACCESS EXCLUSIVE,
// because the run's size check reads every partition under ACCESS SHARE.
func TestMigrateSetTablespaceOnAPartitionedParentDoesNotWaitForAPartitionLock(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, sql := range []string{
		fmt.Sprintf(`
			CREATE TABLE %[1]s.events (
				id bigint,
				created_at timestamptz,
				PRIMARY KEY (id, created_at)
			) PARTITION BY RANGE (created_at)`, schema),
		fmt.Sprintf(`CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')`, schema),
	} {
		_, err := pool.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, holder.Rollback(context.WithoutCancel(t.Context()))) }()
	_, err = holder.Exec(t.Context(), "LOCK TABLE "+schema+".events_2024 IN EXCLUSIVE MODE")
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.events SET TABLESPACE pg_default", schema))
	cmd.LockTimeout = 200 * time.Millisecond
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
}
