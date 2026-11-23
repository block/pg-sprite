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
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// An enum type file that lists labels the live type lacks: diff prints a
// plan for the type with one add-enum-value statement per label and changes
// nothing, migrate --desired adds them, and both then report the type
// converged.
func TestDesiredEnumTypeFileDiffsAndConverges(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.order_status AS ENUM ('new', 'shipped')`, schema))
	require.NoError(t, err)
	const file = "CREATE TYPE order_status AS ENUM ('new', 'paid', 'shipped', 'delivered');\n"

	diff := newDiffCmd(t, url, schema, file)
	diff.JSON = true
	var out strings.Builder
	require.NoError(t, diff.run(t.Context(), &out))
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Equal(t, plan.FormatVersion, report.FormatVersion)
	assert.Equal(t, "order_status", report.Type)
	assert.Empty(t, report.Table)
	require.Len(t, report.Statements, 2)
	assert.Equal(t, fmt.Sprintf("ALTER TYPE %s.order_status ADD VALUE 'paid' BEFORE 'shipped'", schema), report.Statements[0].SQL)
	assert.Equal(t, fmt.Sprintf("ALTER TYPE %s.order_status ADD VALUE 'delivered'", schema), report.Statements[1].SQL)
	for _, st := range report.Statements {
		assert.Equal(t, schemadiff.ChangeAddEnumValue, st.Kind)
	}
	live, err := schemadiff.IntrospectEnumType(t.Context(), pool, schema, "order_status")
	require.NoError(t, err)
	assert.Equal(t, []string{"new", "shipped"}, live.Labels, "diff changes nothing")

	migrateCmd := desiredCommand(t, url, schema, file)
	out.Reset()
	require.NoError(t, migrateCmd.run(t.Context(), &out))
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	assert.Len(t, result.Verdicts, 2)
	live, err = schemadiff.IntrospectEnumType(t.Context(), pool, schema, "order_status")
	require.NoError(t, err)
	assert.Equal(t, []string{"new", "paid", "shipped", "delivered"}, live.Labels)

	diff.JSON, diff.SQL = false, true
	out.Reset()
	require.NoError(t, diff.run(t.Context(), &out))
	assert.Equal(t, fmt.Sprintf("-- no changes: enum type %s.order_status carries every label the file lists\n", schema), out.String())
}

// An enum type file that dropped a live label is refused by diff and by
// migrate --desired with the refusal verdict and exit 2, classed for the
// operator who renames the label directly, and nothing is added.
func TestDesiredEnumTypeFileRefusesADroppedLabel(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.order_status AS ENUM ('new', 'paid')`, schema))
	require.NoError(t, err)
	const file = "CREATE TYPE order_status AS ENUM ('new', 'shipped');\n"

	diff := newDiffCmd(t, url, schema, file)
	diff.JSON = true
	var out strings.Builder
	require.ErrorIs(t, diff.run(t.Context(), &out), verdict.ErrRefused)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v), out.String())
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassNoOnlineSafetyProblem, v.Class)
	assert.Equal(t, verdict.OwnerDirectOperator, v.Owner)
	assert.Equal(t, "CREATE TYPE order_status AS ENUM ('new', 'shipped')", v.Statement)
	assert.Contains(t, v.Detail, "carries ('paid'), which the file no longer lists")

	out.Reset()
	require.ErrorIs(t, desiredCommand(t, url, schema, file).run(t.Context(), &out), verdict.ErrRefused)
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result), out.String())
	assert.Equal(t, verdict.OutcomeRefused, result.Outcome)
	assert.Equal(t, verdict.OwnerDirectOperator, result.Owner)
	live, err := schemadiff.IntrospectEnumType(t.Context(), pool, schema, "order_status")
	require.NoError(t, err)
	assert.Equal(t, []string{"new", "paid"}, live.Labels, "nothing was added")
}

// migrate --desired --dry-run on an enum type file prints the plan diff
// prints for the same file and adds nothing. A file the type cannot
// converge onto is refused at planning with the refusal verdict and exit 2,
// and the live labels stay as they were.
func TestDesiredEnumTypeFileDryRun(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("prints the plan diff prints and adds nothing", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.order_status AS ENUM ('new', 'shipped')`, schema))
		require.NoError(t, err)
		const file = "CREATE TYPE order_status AS ENUM ('new', 'paid', 'shipped', 'delivered');\n"

		diff := newDiffCmd(t, url, schema, file)
		diff.JSON = true
		var diffOut strings.Builder
		require.NoError(t, diff.run(t.Context(), &diffOut))
		var want plan.Report
		require.NoError(t, json.Unmarshal([]byte(diffOut.String()), &want))
		require.Len(t, want.Statements, 2)

		cmd := desiredCommand(t, url, schema, file)
		cmd.DryRun = true
		var out strings.Builder
		require.NoError(t, cmd.run(t.Context(), &out))
		var got plan.Report
		require.NoError(t, json.Unmarshal([]byte(out.String()), &got), out.String())
		assert.Equal(t, want, got, "the dry run prints the plan diff prints")
		assert.Equal(t, "order_status", got.Type)

		live, err := schemadiff.IntrospectEnumType(t.Context(), pool, schema, "order_status")
		require.NoError(t, err)
		assert.Equal(t, []string{"new", "shipped"}, live.Labels, "the dry run adds nothing")
	})

	t.Run("refuses a file the type cannot converge onto", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.order_status AS ENUM ('new', 'paid')`, schema))
		require.NoError(t, err)

		cmd := desiredCommand(t, url, schema, "CREATE TYPE order_status AS ENUM ('new', 'shipped');\n")
		cmd.DryRun = true
		var out strings.Builder
		err = cmd.run(t.Context(), &out)
		require.ErrorIs(t, err, verdict.ErrRefused, "ErrRefused is the exit-2 refusal")
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v), out.String())
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
		assert.Equal(t, verdict.ClassNoOnlineSafetyProblem, v.Class)
		assert.Equal(t, verdict.OwnerDirectOperator, v.Owner)
		assert.Contains(t, v.Detail, "carries ('paid'), which the file no longer lists")

		live, err := schemadiff.IntrospectEnumType(t.Context(), pool, schema, "order_status")
		require.NoError(t, err)
		assert.Equal(t, []string{"new", "paid"}, live.Labels, "nothing was added")
	})
}
