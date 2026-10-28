package cli

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// newOrdersTable creates the table the destructive-change tests drop a
// column from, with rows whose values the drop would discard, and returns
// the database URL, a pool on it, and the table's schema.
func newOrdersTable(t *testing.T) (string, *pgxpool.Pool, string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id   bigint PRIMARY KEY,
			note text
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.orders (id, note)
		SELECT g, 'note ' || g
		FROM generate_series(1, 100) g`, schema))
	require.NoError(t, err)
	return url, pool, schema
}

func noteColumnExists(t *testing.T, pool *pgxpool.Pool, schema string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1
			  AND table_name = 'orders'
			  AND column_name = 'note')`,
		schema).Scan(&exists))
	return exists
}

// Without --allow-destructive, a DROP COLUMN is refused with the refusal
// exit and the destructive-change verdict, and the column keeps its values.
func TestMigrateRefusesAnUnacknowledgedDropColumn(t *testing.T) {
	url, pool, schema := newOrdersTable(t)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.orders DROP COLUMN note", schema))
	cmd.JSON = true
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonDestructiveChange, v.Reason)
	assert.Equal(t, verdict.ClassByDesign, v.Class)
	assert.False(t, v.AllowedDestructive)
	assert.True(t, noteColumnExists(t, pool, schema), "nothing was executed")
}

// With --allow-destructive naming the table, the drop executes with the
// online-safe exit and the verdict records the acknowledgement.
func TestMigrateRunsAnAcknowledgedDropColumn(t *testing.T) {
	url, pool, schema := newOrdersTable(t)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.orders DROP COLUMN note", schema))
	cmd.AllowDestructive = schema + ".orders"
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.AllowedDestructive)
	assert.False(t, noteColumnExists(t, pool, schema))
}

// A dry run ends in the refusal the apply would: the destructive-change
// verdict and the refusal exit, with no plan report. With the
// acknowledgement it reports the plan the acknowledged apply takes, and
// executes nothing either way.
func TestMigrateDryRunReportsTheDestructiveRefusal(t *testing.T) {
	url, pool, schema := newOrdersTable(t)
	alter := fmt.Sprintf("ALTER TABLE %s.orders DROP COLUMN note", schema)

	refused := newMigrateCmd(url, alter)
	refused.DryRun = true
	refused.JSON = true
	var out strings.Builder
	require.ErrorIs(t, refused.run(t.Context(), &out), verdict.ErrRefused)
	var wire map[string]any
	require.NoError(t, json.Unmarshal([]byte(out.String()), &wire))
	assert.Equal(t, string(verdict.OutcomeRefused), wire["outcome"])
	assert.Equal(t, string(verdict.ReasonDestructiveChange), wire["reason"])
	assert.Equal(t, string(verdict.ClassByDesign), wire["class"])
	assert.NotContains(t, wire, "format_version", "the refusal is a verdict, not a plan report")

	acknowledged := newMigrateCmd(url, alter)
	acknowledged.AllowDestructive = schema + ".orders"
	report := dryRunPlanOf(t, acknowledged)
	require.Len(t, report.Statements, 1)
	assert.True(t, report.Statements[0].Destructive)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	assert.Equal(t, plan.SourceAlter, report.Source)

	assert.True(t, noteColumnExists(t, pool, schema), "a dry run executes nothing")
}

// An unqualified table is acknowledged by the schema the session's
// search_path resolves it to. Two schemas hold an orders table, and the
// search_path puts the second one first: naming the other schema's orders
// is a usage error from the dry run and the apply alike, with neither
// table changed, and naming the resolved one drops the column from that
// table only.
func TestMigrateAcknowledgesTheTableTheSearchPathResolves(t *testing.T) {
	url, pool, other := newOrdersTable(t)
	resolved := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id   bigint PRIMARY KEY,
			note text
		)`, resolved))
	require.NoError(t, err)
	u, err := neturl.Parse(url)
	require.NoError(t, err)
	q := u.Query()
	q.Set("options", "-csearch_path="+resolved+","+other)
	u.RawQuery = q.Encode()
	const alter = "ALTER TABLE orders DROP COLUMN note"

	for _, dryRun := range []bool{true, false} {
		cmd := newMigrateCmd(u.String(), alter)
		cmd.DryRun = dryRun
		cmd.AllowDestructive = other + ".orders"
		var out strings.Builder
		err := cmd.run(t.Context(), &out)
		require.ErrorIs(t, err, migrate.ErrAllowDestructiveMismatch, "dry run %v", dryRun)
		assert.Contains(t, err.Error(), resolved+".orders", "the error names the table the search_path resolves")
		assert.Empty(t, out.String())
	}
	assert.True(t, noteColumnExists(t, pool, other), "the other schema's table is untouched")
	assert.True(t, noteColumnExists(t, pool, resolved), "a mismatched acknowledgement executes nothing")

	cmd := newMigrateCmd(u.String(), alter)
	cmd.AllowDestructive = resolved + ".orders"
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.AllowedDestructive)
	assert.False(t, noteColumnExists(t, pool, resolved), "the resolved table lost the column")
	assert.True(t, noteColumnExists(t, pool, other), "the other schema's table kept it")
}

// A dry run checks the acknowledgement the way the apply does: one that
// names another table is a usage error with nothing printed.
func TestMigrateDryRunRejectsAMismatchedDestructiveAcknowledgement(t *testing.T) {
	url, _, schema := newOrdersTable(t)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.orders DROP COLUMN note", schema))
	cmd.DryRun = true
	cmd.AllowDestructive = "public.orders"
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, migrate.ErrAllowDestructiveMismatch)
	assert.Empty(t, out.String())
}
