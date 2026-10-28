package preflight

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// A partitioned table whose partitions were all scanned before the window
// opened, or never scanned since a reset before it, is idle. A real server
// cannot age a scan past the shortest window Judge accepts within a test,
// so this judges the real evidence against a window that opens at the
// read's own time: every scan and the reset were published before the
// read began, so all of them are before that window. The floor on Judge's
// window is about scans not yet published, and the test publishes its
// scans and waits to see them before reading.
func TestReadTableUsageOfAPartitionedTableIdleSinceAReset(t *testing.T) {
	dbURL := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: dbURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	major, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)
	if major < 16 {
		t.Skipf("PostgreSQL %d records no scan times", major)
	}
	schema := testutil.NewSchema(t, pool)
	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	exec := func(sql string) {
		t.Helper()
		_, err := conn.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}
	exec(fmt.Sprintf(`
		CREATE TABLE %s.events (
			id bigint
		) PARTITION BY RANGE (id)`, schema))
	exec(fmt.Sprintf(`CREATE TABLE %s.events_2025 PARTITION OF %s.events FOR VALUES FROM (0) TO (100)`, schema, schema))
	exec(fmt.Sprintf(`CREATE TABLE %s.events_2026 PARTITION OF %s.events FOR VALUES FROM (100) TO (200)`, schema, schema))
	// The reset gives the database a reset time, so a partition never
	// scanned since is provably unscanned.
	exec("SELECT pg_stat_reset()")
	exec(fmt.Sprintf(`SELECT count(*) FROM %s.events_2025`, schema))
	exec("SELECT pg_stat_force_next_flush()")

	var u TableUsage
	var readErr error
	require.Eventually(t, func() bool {
		u, readErr = ReadTableUsage(t.Context(), pool, schema, "events")
		return readErr != nil || u.SeqScans() == 1
	}, 10*time.Second, 100*time.Millisecond, "the server must publish the scan of events_2025")
	require.NoError(t, readErr)

	_, reset := u.StatsReset()
	require.True(t, reset)
	partitions := u.Partitions()
	require.Len(t, partitions, 2)
	_, scanned := partitions[0].LastSeqScan()
	assert.True(t, scanned, "events_2025 was scanned")
	assert.True(t, partitions[1].neverScanned(), "events_2026 was never scanned")
	outcome, reason, leaf := u.judge(u.ReadAt())
	assert.Equal(t, UsageIdle, outcome)
	assert.Equal(t, UsageReasonNoScanInWindow, reason)
	assert.Equal(t, noLeaf, leaf)
}
