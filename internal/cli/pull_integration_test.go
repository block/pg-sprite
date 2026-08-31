package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/lint"
	"github.com/block/pg-sprite/pkg/verdict"
)

func TestPullExportsAllRenderableTablesAndReportsRefusals(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.accounts (id bigint PRIMARY KEY, name text NOT NULL)", schema),
		fmt.Sprintf("CREATE TABLE %s.events (id bigint PRIMARY KEY, created_at timestamptz DEFAULT now())", schema),
		fmt.Sprintf("CREATE INDEX events_created_at_idx ON %s.events (created_at)", schema),
		fmt.Sprintf("CREATE TABLE %s.metrics (id bigint, day date) PARTITION BY RANGE (day)", schema),
		fmt.Sprintf("CREATE TABLE %s.metrics_2026 PARTITION OF %s.metrics FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')", schema, schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	outDir := filepath.Join(t.TempDir(), "pulled")
	cmd := &PullCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Out: outDir}
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)

	assert.Contains(t, out.String(), "PULLED  accounts -> ")
	assert.Contains(t, out.String(), "PULLED  events -> ")
	assert.Contains(t, out.String(), "REFUSED metrics: render table \"metrics\": partitioned parent")
	assert.NotContains(t, out.String(), "metrics_2026")
	assert.Contains(t, out.String(), "Summary: 2 pulled, 1 refused, 0 errors")

	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "accounts.sql", entries[0].Name())
	assert.Equal(t, "events.sql", entries[1].Name())
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(outDir, entry.Name()))
		require.NoError(t, err)
		report, err := lint.Check(string(raw))
		require.NoError(t, err, entry.Name())
		assert.Zero(t, report.Errors, entry.Name())
	}
}

func TestPullRefusesBothSidesOfClassicInheritance(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.parent (id bigint NOT NULL)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.child (note text) INHERITS (%s.parent)", schema, schema))
	require.NoError(t, err)

	var out strings.Builder
	err = (&PullCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Out: t.TempDir()}).run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Contains(t, out.String(), "REFUSED child:")
	assert.Contains(t, out.String(), "REFUSED parent:")
	assert.Contains(t, out.String(), "Summary: 0 pulled, 2 refused, 0 errors")
}

func TestPullRejectsMissingSchema(t *testing.T) {
	url := testutil.StartPostgres(t)
	outDir := filepath.Join(t.TempDir(), "must-not-exist")
	var out strings.Builder
	err := (&PullCmd{DBFlags: DBFlags{URL: url}, Schema: "missing_schema", Out: outDir}).run(t.Context(), &out)
	require.ErrorContains(t, err, `schema "missing_schema" does not exist`)
	assert.NoDirExists(t, outDir)
	assert.Empty(t, out.String())
}
