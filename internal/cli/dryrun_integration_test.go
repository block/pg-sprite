package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// dryRunPlan runs migrate --dry-run --json and decodes the plan report.
func dryRunPlan(t *testing.T, url, alter string) plan.Report {
	t.Helper()
	cmd := newMigrateCmd(url, alter)
	cmd.DryRun = true
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	require.Equal(t, plan.FormatVersion, report.FormatVersion)
	require.Equal(t, plan.SourceAlter, report.Source)
	return report
}

// A rewrite-requiring change dry-runs to the copy-and-swap backend as
// unavailable, and nothing executes: the live column type is untouched.
func TestMigrateDryRunRoutesRewriteWithoutExecuting(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	report := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	assert.Equal(t, router.DispositionUnavailable, report.Disposition)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, planner.RouteCopyAndSwap, st.Route)
	assert.Equal(t, router.BackendCopyAndSwap, st.Backend)
	assert.Empty(t, st.ExecSQL)

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "integer", typ, "dry-run must not execute the change")
}

func TestMigrateDryRunRefusesPartitionedParentIndexPlan(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := createPartitionFixture(t, pool)
	report := dryRunPlan(t, url, fmt.Sprintf("CREATE INDEX p_v_idx ON %s.p (v)", schema))
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, report.Reason)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, router.DispositionRefuse, report.Statements[0].Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, report.Statements[0].Reason)
	assert.Empty(t, report.Statements[0].ExecSQL)
	for _, decision := range report.Statements[0].Decisions {
		assert.Empty(t, decision.SaferSQL)
		assert.Empty(t, decision.SaferSQLExecution)
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
func TestDryRunAndDiffAgreeOnDestructive(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, doomed text)", schema))
	require.NoError(t, err)

	alterReport := dryRunPlan(t, url, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN doomed", schema))
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
	assert.Equal(t, alterReport.Fingerprint, diffReport.Fingerprint,
		"the same plan must carry the same identity through either door")
	assert.NotEmpty(t, alterReport.Fingerprint)
}

// An unqualified statement resolves to the schema the engine actually
// introspected — public — and the report says so: a stored plan must not
// depend on the reader's search_path to name its target. The report also
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
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.ReasonBinaryCoercible, report.Statements[0].Decisions[0].Reason,
		"resolving to public must feed the live facts to the classifier")
}

// A dry-run against a table that does not exist classifies with zero facts:
// the unprovable type change routes conservatively instead of failing.
func TestMigrateDryRunMissingTableIsConservative(t *testing.T) {
	url := testutil.StartPostgres(t)

	report := dryRunPlan(t, url, "ALTER TABLE missing ALTER COLUMN v TYPE varchar(50)")
	assert.Equal(t, router.DispositionUnavailable, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, report.Statements[0].Route)
}
