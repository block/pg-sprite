package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// ordersFixture creates schema.orders with rows in it, whose qty column a
// retype to bigint rewrites, so the planner routes the retype to
// copy-and-swap.
func ordersFixture(t *testing.T) (url, schema string, pool *pgxpool.Pool) {
	t.Helper()
	url = testutil.StartPostgres(t)
	p, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(p.Close)
	schema = testutil.NewSchema(t, p)
	_, err = p.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id  bigint PRIMARY KEY,
			qty integer NOT NULL
		)`, schema))
	require.NoError(t, err)
	_, err = p.Exec(t.Context(), fmt.Sprintf(
		"INSERT INTO %s.orders SELECT g, g %% 10 FROM generate_series(1, 1000) g", schema))
	require.NoError(t, err)
	return url, schema, p
}

// qtyType reads the live type of schema.orders.qty.
func qtyType(t *testing.T, pool *pgxpool.Pool, schema string) string {
	t.Helper()
	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'qty'`, schema).Scan(&typ))
	return typ
}

// A retype from integer to bigint rewrites every row, so it routes to
// copy-and-swap, which is in preview and closed unless the run opens it
// with --copy-and-swap. Without the flag the run refuses the retype before
// anything executes, and the dry run must say the same: the run's refusal
// verdict, cause copy-and-swap-not-enabled, and the refusal exit code, so
// a CI gate on the dry run does not pass a change its run refuses.
func TestMigrateDryRunRefusesAClosedCopySwapRetypeLikeApply(t *testing.T) {
	url, schema, pool := ordersFixture(t)
	alter := fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN qty TYPE bigint", schema)

	run := func(cmd *MigrateCmd) verdict.Verdict {
		cmd.JSON = true
		var out strings.Builder
		require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused,
			"the refusal exit code must not depend on --dry-run")
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		return v
	}
	dry := newMigrateCmd(url, alter)
	dry.DryRun = true
	dryRun := run(dry)
	applied := run(newMigrateCmd(url, alter))

	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, dryRun.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, dryRun.Class)
	assert.Equal(t, verdict.CauseCopyAndSwapNotEnabled, dryRun.Cause)
	assert.Equal(t, schema+".orders", dryRun.Table)
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the run reports")
	assert.Equal(t, "integer", qtyType(t, pool, schema), "neither the dry run nor the refused run changed the column")
}

// The same retype with --copy-and-swap: the dry run reports the plan the
// opened route runs, executable on the copy-and-swap backend with exit 0,
// and the run with the flag (offline, so the test server needs no logical
// decoding) executes it through that route.
func TestMigrateDryRunPlansAnOpenedCopySwapRetypeLikeApply(t *testing.T) {
	url, schema, pool := ordersFixture(t)
	alter := fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN qty TYPE bigint", schema)

	dry := newMigrateCmd(url, alter)
	dry.CopyAndSwap = true
	report := dryRunPlanOf(t, dry)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, report.Statements[0].Route)
	assert.Equal(t, router.BackendCopyAndSwap, report.Statements[0].Backend)
	assert.Equal(t, router.DispositionExecute, report.Statements[0].Disposition)
	assert.Equal(t, "integer", qtyType(t, pool, schema), "the dry run executes nothing")

	apply := newMigrateCmd(url, alter)
	apply.CopyAndSwap = true
	apply.FreeDisk = byteSize(testutil.UnlimitedDisk)
	apply.BlockWrites = schema + ".orders"
	apply.JSON = true
	var out strings.Builder
	require.NoError(t, apply.run(t.Context(), &out))
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, "bigint", qtyType(t, pool, schema))
}

// A desired file that retypes qty plans the retype through copy-and-swap,
// and the run refuses the whole plan while the route is closed; the
// desired-state dry run ends in that same result, and with --copy-and-swap
// previews the plan instead.
func TestMigrateDesiredDryRunRefusesAClosedCopySwapPlanLikeApply(t *testing.T) {
	url, schema, pool := ordersFixture(t)
	desired := `CREATE TABLE orders (
 id bigint PRIMARY KEY,
 qty bigint NOT NULL
 );`

	run := func(cmd *MigrateCmd) migrate.DesiredResult {
		var out strings.Builder
		require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused,
			"the refusal exit code must not depend on --dry-run")
		var result migrate.DesiredResult
		require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
		return result
	}
	dryRun := run(desiredCommand(t, url, schema, desired, "--dry-run"))
	applied := run(desiredCommand(t, url, schema, desired))

	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, dryRun.Reason)
	assert.Equal(t, verdict.CauseCopyAndSwapNotEnabled, dryRun.Cause)
	assert.Empty(t, dryRun.Verdicts, "whole-plan admission runs no statement")
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the run reports")

	var out strings.Builder
	require.NoError(t, desiredCommand(t, url, schema, desired, "--dry-run", "--copy-and-swap").run(t.Context(), &out))
	var preview plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &preview))
	assert.Equal(t, router.DispositionExecute, preview.Disposition)
	require.Len(t, preview.Statements, 1)
	assert.Equal(t, router.BackendCopyAndSwap, preview.Statements[0].Backend)
	assert.Equal(t, "integer", qtyType(t, pool, schema), "nothing ran")
}
