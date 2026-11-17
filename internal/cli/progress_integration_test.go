package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/verdict"
)

// testProgressInterval polls often enough that a change on a small table
// is observed while it runs.
const testProgressInterval = 5 * time.Millisecond

// progressLabels are the operations a concurrent index build's run can
// report, as the progress line names them.
func progressLabels() []string {
	return []string{
		operationLabel(""),
		operationLabel(progress.OperationAdmitting),
		operationLabel(progress.OperationOptimistic),
		operationLabel(progress.OperationConcurrentIndex),
	}
}

// migrate --progress=always on a real change prints its progress to stderr
// as plain lines while the change runs, and stdout carries the verdict
// alone. A blocking CREATE INDEX is substituted with a concurrent build, so
// the run has a measurable step to report.
func TestMigrateProgressAlwaysWritesProgressToStderr(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.t (
		id int PRIMARY KEY,
		c  int NOT NULL
	)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.t (id, c)
		SELECT g, g %% 1000 FROM generate_series(1, 200000) AS g`, schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX t_c_idx ON %s.t (c)", schema))
	cmd.Progress = "always"
	cmd.progressInterval = testProgressInterval
	var stderr syncWriter
	cmd.diagOut = &stderr
	var stdout strings.Builder
	require.NoError(t, cmd.run(t.Context(), &stdout))

	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	require.NotEmpty(t, stderr.String(), "the run must print at least one progress line on stderr")
	assert.NotContains(t, stderr.String(), "\r", "a non-terminal stderr gets plain lines, never redraws")
	for _, line := range lines {
		assert.True(t, hasAnyPrefix(line, progressLabels()), "a progress line names the run's operation: %q", line)
		assert.NotContains(t, stdout.String(), line, "progress never reaches stdout")
	}
	assert.NotEmpty(t, stdout.String(), "stdout carries the verdict")

	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT i.indisvalid FROM pg_catalog.pg_index i
		 JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		 JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = 't_c_idx'`, schema).Scan(&valid))
	assert.True(t, valid)
}

// With --json the run shows no progress even when asked for it on a path
// that bypasses flag validation: stderr stays empty and stdout decodes as
// the verdict alone.
func TestMigrateProgressAbsentWithJSON(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	cmd.JSON = true
	cmd.Progress = "always"
	cmd.progressInterval = testProgressInterval
	var stderr syncWriter
	cmd.diagOut = &stderr
	var stdout strings.Builder
	require.NoError(t, cmd.run(t.Context(), &stdout))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(stdout.String()), &v), "stdout must carry exactly the verdict")
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Empty(t, stderr.String())
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
