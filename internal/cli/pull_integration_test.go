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
