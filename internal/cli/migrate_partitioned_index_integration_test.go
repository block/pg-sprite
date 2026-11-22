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
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/verdict"
)

// partitionedIndexFixture creates a range-partitioned table `p` with two
// partitions and the given number of rows in each, returning the schema.
func partitionedIndexFixture(t *testing.T, pool *pgxpool.Pool, rowsPerPartition int) string {
	t.Helper()
	schema := createPartitionFixture(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.p2 PARTITION OF %s.p FOR VALUES FROM (100) TO (200)", schema, schema))
	require.NoError(t, err)
	if rowsPerPartition > 0 {
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			`INSERT INTO %[1]s.p1 (id, v) SELECT g %% 100, md5(g::text) FROM generate_series(1, %[2]d) g;
			 INSERT INTO %[1]s.p2 (id, v) SELECT 100 + g %% 100, md5(g::text) FROM generate_series(1, %[2]d) g`,
			schema, rowsPerPartition))
		require.NoError(t, err)
	}
	return schema
}

// indexState is one index's validity and whether it is attached as the child
// of another index.
type indexState struct {
	valid    bool
	attached bool
}

// indexStates returns the non-primary-key index of the parent and of each
// partition, keyed by the table the index is on.
func indexStates(t *testing.T, pool *pgxpool.Pool, schema string) map[string]indexState {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT t.relname, i.indisvalid,
		       EXISTS (SELECT 1 FROM pg_inherits h WHERE h.inhrelid = i.indexrelid)
		FROM pg_index i
		JOIN pg_class t ON t.oid = i.indrelid
		WHERE t.relnamespace = $1::regnamespace AND t.relname IN ('p', 'p1', 'p2')
		  AND NOT i.indisprimary
		ORDER BY t.relname`, schema)
	require.NoError(t, err)
	defer rows.Close()
	states := map[string]indexState{}
	for rows.Next() {
		var table string
		var s indexState
		require.NoError(t, rows.Scan(&table, &s.valid, &s.attached))
		states[table] = s
	}
	require.NoError(t, rows.Err())
	return states
}

func assertIndexBuiltEverywhere(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	states := indexStates(t, pool, schema)
	require.Len(t, states, 3, "the parent and both partitions each carry one index")
	assert.True(t, states["p"].valid, "the parent index is valid")
	assert.False(t, states["p"].attached, "the parent index is not attached to anything")
	for _, partition := range []string{"p1", "p2"} {
		assert.True(t, states[partition].valid, "%s index is valid", partition)
		assert.True(t, states[partition].attached, "%s index is attached to the parent index", partition)
	}
}

func decodeVerdict(t *testing.T, out string) verdict.Verdict {
	t.Helper()
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out), &v), "stdout must carry the verdict: %q", out)
	return v
}

// Running a plain CREATE INDEX on a partitioned table through --alter builds
// the index one partition at a time and reports it as executed. The parent's
// index is valid and every partition's index is attached when the run ends.
func TestMigrateAlterBuildsAPartitionedParentIndex(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedIndexFixture(t, pool, 1000)

	cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX p_v_idx ON %s.p (v)", schema))
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	v := decodeVerdict(t, out.String())
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, schema+".p", v.Table)
	assert.Contains(t, v.Detail, "partition by partition")
	assert.Contains(t, v.Detail, "2 partitions, 2 given an index by this run")
	assertIndexBuiltEverywhere(t, pool, schema)

	// Running the statement again is safe.
	out.Reset()
	again := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX p_v_idx ON %s.p (v)", schema))
	again.JSON = true
	require.NoError(t, again.run(t.Context(), &out))
	rerun := decodeVerdict(t, out.String())
	assert.Equal(t, verdict.OutcomeExecuted, rerun.Outcome)
	assert.Contains(t, rerun.Detail, "2 partitions, 0 given an index by this run",
		"running the statement again finds the finished index and changes nothing")
}

// --force never bypasses the partitioned parent's refusal: a forced plain
// CREATE INDEX is still refused and builds nothing, because the forced form
// would take the parent's blocking build.
func TestMigrateAlterForcedPartitionedParentIndexStaysRefused(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedIndexFixture(t, pool, 0)

	cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX p_v_idx ON %s.p (v)", schema))
	cmd.JSON = true
	cmd.Force = schema + ".p"
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	v := decodeVerdict(t, out.String())
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, v.Reason)
	assert.Empty(t, indexStates(t, pool, schema), "a refusal builds no index")
}

// An interrupted build leaves the parent's index invalid and some partitions
// indexed. Running the same statement again adopts what is there and builds
// only what is missing.
func TestMigrateAlterResumesAnUnfinishedPartitionedIndex(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedIndexFixture(t, pool, 1000)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		`CREATE INDEX p_v_idx ON ONLY %[1]s.p (v);
		 CREATE INDEX p1_v_idx ON %[1]s.p1 (v);
		 ALTER INDEX %[1]s.p_v_idx ATTACH PARTITION %[1]s.p1_v_idx`, schema))
	require.NoError(t, err)
	before := indexStates(t, pool, schema)
	require.False(t, before["p"].valid, "the interrupted build left the parent index invalid")
	require.True(t, before["p1"].attached)

	cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX p_v_idx ON %s.p (v)", schema))
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	v := decodeVerdict(t, out.String())
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Contains(t, v.Detail, "2 partitions, 1 given an index by this run",
		"the finished partition's index is adopted, only the missing one is built")
	assertIndexBuiltEverywhere(t, pool, schema)
}

// With --progress=always the partitioned build prints plain progress lines on
// stderr as it works, and stdout carries the verdict alone.
func TestMigrateAlterPartitionedIndexReportsProgress(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedIndexFixture(t, pool, 100000)

	cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX p_v_idx ON %s.p (v)", schema))
	cmd.Progress = "always"
	cmd.progressInterval = testProgressInterval
	var stderr syncWriter
	cmd.diagOut = &stderr
	var stdout strings.Builder
	require.NoError(t, cmd.run(t.Context(), &stdout))

	require.NotEmpty(t, stderr.String(), "the run must print progress lines on stderr")
	assert.NotContains(t, stderr.String(), "\r", "a non-terminal stderr gets plain lines, never redraws")
	labels := append(progressLabels(), operationLabel(progress.OperationOptimistic))
	for line := range strings.SplitSeq(strings.TrimSuffix(stderr.String(), "\n"), "\n") {
		assert.True(t, hasAnyPrefix(line, labels), "a progress line names the run's operation: %q", line)
	}
	assert.Contains(t, stderr.String(), operationLabel(progress.OperationConcurrentIndex),
		"the partitions' concurrent builds are reported")
	assert.NotContains(t, stdout.String(), operationLabel(progress.OperationConcurrentIndex), "progress never reaches stdout")
	assert.True(t, strings.HasPrefix(stdout.String(), "executed natively"), "stdout carries the verdict: %q", stdout.String())
	assertIndexBuiltEverywhere(t, pool, schema)
}
