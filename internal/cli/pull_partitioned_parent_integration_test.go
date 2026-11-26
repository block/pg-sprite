package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
)

// pull writes a partitioned parent's definition — PARTITION BY clause and
// indexes, no partitions — and diff of the pulled file against the same
// live parent plans nothing: the file is a baseline for the parent, while
// its partitions stay owned outside the desired file.
func TestPullPartitionedParentDiffsToNoChanges(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			community_id bigint NOT NULL,
			created_at timestamptz NOT NULL,
			body text NOT NULL,
			CONSTRAINT events_pkey PRIMARY KEY (community_id, created_at, id)
		) PARTITION BY RANGE (created_at);

		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');

		CREATE INDEX events_community_created_idx ON %[1]s.events (community_id, created_at DESC);`, schema))
	require.NoError(t, err)

	outDir := filepath.Join(t.TempDir(), "pulled")
	var pullOut strings.Builder
	require.NoError(t, (&PullCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Out: outDir}).run(t.Context(), &pullOut))

	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the partitions are not pulled")
	pulled := filepath.Join(outDir, "events.sql")
	raw, err := os.ReadFile(pulled)
	require.NoError(t, err)
	assert.Equal(t, `CREATE TABLE "events" (
    "id" bigint NOT NULL,
    "community_id" bigint NOT NULL,
    "created_at" timestamp with time zone NOT NULL,
    "body" text NOT NULL,
    CONSTRAINT "events_pkey" PRIMARY KEY (community_id, created_at, id)
) PARTITION BY RANGE (created_at);

CREATE INDEX events_community_created_idx ON events USING btree (community_id, created_at DESC);
`, string(raw))

	diff := &DiffCmd{
		DBFlags: DBFlags{
			URL:              url,
			LockTimeout:      3 * time.Second,
			StatementTimeout: 30 * time.Second,
		},
		Desired: pulled,
		Schema:  schema,
		JSON:    true,
	}
	var diffOut strings.Builder
	require.NoError(t, diff.run(t.Context(), &diffOut))
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(diffOut.String()), &report))
	require.NotNil(t, report.TableExists)
	assert.True(t, *report.TableExists)
	assert.Empty(t, report.Statements, "the pulled parent must diff to no changes against itself")
}
