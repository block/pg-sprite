package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
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

// eventsDesiredWithoutKindIndex is the desired file for events once its
// events_kind_idx is removed. The partitions are not part of the file.
const eventsDesiredWithoutKindIndex = `CREATE TABLE events (
    id bigint NOT NULL,
    kind text NOT NULL,
    created_at timestamp with time zone NOT NULL
) PARTITION BY RANGE (created_at);`

// eventsIndexCount counts the indexes in the schema: the parent's
// events_kind_idx and each partition's index attached under it.
func eventsIndexCount(t *testing.T, pool *pgxpool.Pool, schema string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_indexes WHERE schemaname = $1`, schema).Scan(&n))
	return n
}

// An operator removes events_kind_idx from a partitioned table's desired
// file. PostgreSQL has no concurrent drop for that index, so the diff shows
// the plain DROP INDEX as the refused blocking drop, a run without
// --accept-blocking is refused as before, an acceptance for another table
// or without --statement-timeout stops before anything runs, and the run
// with every flag drops the parent's index and each partition's index and
// exits with the accepted-blocking code.
func TestMigrateDesiredDropsPartitionedIndexUnderAcceptedBlocking(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			kind text NOT NULL,
			created_at timestamptz NOT NULL
		) PARTITION BY RANGE (created_at);

		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
		CREATE TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');

		CREATE INDEX events_kind_idx ON %[1]s.events (kind);`, schema))
	require.NoError(t, err)
	require.Equal(t, 4, eventsIndexCount(t, pool, schema), "the parent's index and one per partition")
	table := schema + ".events"
	drop := fmt.Sprintf("DROP INDEX %s.events_kind_idx", schema)

	cmd := desiredCommand(t, url, schema, eventsDesiredWithoutKindIndex)
	diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: cmd.Desired, JSON: true}
	var out strings.Builder
	require.ErrorIs(t, diff.run(t.Context(), &out), verdict.ErrRefused, "the plan stays refused")
	var reviewed plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &reviewed))
	require.Len(t, reviewed.Statements, 1)
	assert.Equal(t, plan.FormatVersion, reviewed.FormatVersion)
	assert.Equal(t, router.DispositionRefuse, reviewed.Statements[0].Disposition)
	assert.Equal(t, []string{drop}, reviewed.Statements[0].ExecSQL)
	assert.Equal(t, planner.ExecutionBlockingIndexDrop, reviewed.Statements[0].Execution)
	accepted := []string{
		"--expect-fingerprint", reviewed.Fingerprint, "--allow-destructive", table,
		"--accept-blocking", table, "--lock-timeout", "1s", "--statement-timeout", "10s",
	}

	t.Run("a dry run without the flags names them, --accept-blocking included", func(t *testing.T) {
		dry := desiredCommand(t, url, schema, eventsDesiredWithoutKindIndex, "--dry-run")
		dry.JSON = false
		var out strings.Builder
		require.ErrorIs(t, dry.run(t.Context(), &out), verdict.ErrRefused)
		assert.Contains(t, out.String(), fmt.Sprintf("--expect-fingerprint %s --allow-destructive %s --accept-blocking %s --statement-timeout DURATION\n",
			reviewed.Fingerprint, table, table))
		assert.Equal(t, 4, eventsIndexCount(t, pool, schema))
	})

	t.Run("a run without --accept-blocking is refused as before", func(t *testing.T) {
		run := desiredCommand(t, url, schema, eventsDesiredWithoutKindIndex,
			"--expect-fingerprint", reviewed.Fingerprint, "--allow-destructive", table)
		var out strings.Builder
		require.ErrorIs(t, run.run(t.Context(), &out), verdict.ErrRefused)
		var result migrate.DesiredResult
		require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
		assert.Equal(t, verdict.ReasonDestructiveChange, result.Reason)
		assert.Equal(t, verdict.ClassByDesign, result.Class)
		assert.Empty(t, result.Verdicts)
		assert.Contains(t, result.Detail, "--accept-blocking "+table)
		assert.Equal(t, 4, eventsIndexCount(t, pool, schema), "nothing was dropped")
	})

	t.Run("an acceptance naming another table stops before anything runs", func(t *testing.T) {
		run := desiredCommand(t, url, schema, eventsDesiredWithoutKindIndex,
			"--expect-fingerprint", reviewed.Fingerprint, "--allow-destructive", table,
			"--accept-blocking", schema+".orders", "--statement-timeout", "10s")
		var out strings.Builder
		require.ErrorIs(t, run.run(t.Context(), &out), migrate.ErrAcceptBlockingMismatch)
		assert.Empty(t, out.String())
		assert.Equal(t, 4, eventsIndexCount(t, pool, schema), "nothing was dropped")
	})

	t.Run("an acceptance without --statement-timeout is a usage error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.sql")
		require.NoError(t, os.WriteFile(path, []byte(eventsDesiredWithoutKindIndex), 0o600))
		parser, err := kong.New(New("test"), kong.Vars{"version": "test"})
		require.NoError(t, err)
		_, err = parser.Parse([]string{"migrate", "--url", url, "--schema", schema, "--desired", path,
			"--expect-fingerprint", reviewed.Fingerprint, "--allow-destructive", table, "--accept-blocking", table})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--accept-blocking requires --statement-timeout")
		assert.Equal(t, 4, eventsIndexCount(t, pool, schema), "nothing was dropped")
	})

	t.Run("the run with every flag drops the tree and exits with the accepted-blocking code", func(t *testing.T) {
		run := desiredCommand(t, url, schema, eventsDesiredWithoutKindIndex, accepted...)
		var out strings.Builder
		require.ErrorIs(t, run.run(t.Context(), &out), verdict.ErrAcceptedBlocking)
		var result migrate.DesiredResult
		require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
		assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, result.Outcome, result.Detail)
		require.Len(t, result.Verdicts, 1)
		v := result.Verdicts[0]
		assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, v.Outcome)
		assert.Equal(t, verdict.CauseParentConcurrentIndexDrop, v.Cause)
		assert.True(t, v.BlockingPassthrough)
		assert.Equal(t, "1s", v.LockTimeout)
		assert.Equal(t, "10s", v.StatementTimeout)
		assert.Equal(t, []string{drop}, v.ExecutedSQL)
		assert.Zero(t, eventsIndexCount(t, pool, schema), "the parent's index and every partition's index are gone")
	})
}
