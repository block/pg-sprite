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

// A dry-run against a table that does not exist classifies with zero facts:
// the unprovable type change routes conservatively instead of failing.
func TestMigrateDryRunMissingTableIsConservative(t *testing.T) {
	url := testutil.StartPostgres(t)

	report := dryRunPlan(t, url, "ALTER TABLE missing ALTER COLUMN v TYPE varchar(50)")
	assert.Equal(t, router.DispositionUnavailable, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, report.Statements[0].Route)
}
