package preflight_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/preflight"
)

// A partitioned index has no statistics of its own: the server records its
// scans on the partitions' indexes. ReadIndexUsage sums them over every
// leaf index at any depth, here 3 lookups in one partition and 2 in a
// sub-partition's partition, and reports the partitioned index's leaves.
func TestReadIndexUsageSumsAPartitionedIndexsLeafIndexes(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id int NOT NULL,
			ts timestamptz NOT NULL,
			k int NOT NULL
		) PARTITION BY RANGE (ts);
		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		CREATE TABLE %[1]s.events_2027 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')
			PARTITION BY RANGE (ts);
		CREATE TABLE %[1]s.events_2027_h1 PARTITION OF %[1]s.events_2027
			FOR VALUES FROM ('2027-01-01') TO ('2027-07-01');
		CREATE INDEX events_k_idx ON %[1]s.events (k);
		INSERT INTO %[1]s.events SELECT g, '2026-03-01', g FROM generate_series(1, 2000) g;
		INSERT INTO %[1]s.events SELECT g, '2027-03-01', g FROM generate_series(1, 2000) g;
		ANALYZE %[1]s.events`, f.schema))

	// Seq scans are disabled so each lookup scans the partition's index.
	f.exec(t, "SET enable_seqscan = off")
	for range 3 {
		f.exec(t, fmt.Sprintf(`
			SELECT id
			FROM %s.events
			WHERE ts = '2026-03-01' AND k = 5`, f.schema))
	}
	for range 2 {
		f.exec(t, fmt.Sprintf(`
			SELECT id
			FROM %s.events
			WHERE ts = '2027-03-01' AND k = 5`, f.schema))
	}
	f.exec(t, "RESET enable_seqscan")
	f.publish(t)

	parent := f.indexOID(t, "events_k_idx")
	var u preflight.IndexUsage
	var readErr error
	require.Eventually(t, func() bool {
		if f.major < 15 {
			if _, readErr = f.conn.Exec(t.Context(), "SELECT 1"); readErr != nil {
				return true
			}
		}
		u, readErr = preflight.ReadIndexUsage(t.Context(), f.pool, parent)
		return readErr != nil || u.IdxScans == 5
	}, statsVisible, 100*time.Millisecond, "the partitions' 5 scans must reach the partitioned index's usage")
	require.NoError(t, readErr)

	assert.EqualValues(t, 5, u.IdxScans)
	assert.True(t, u.Partitioned)
	assert.Equal(t, 2, u.Partitions, "events_2026's index and events_2027_h1's; events_2027's own is partitioned, not a leaf")
	if f.major >= 16 {
		assert.NotNil(t, u.LastIdxScan, "the latest leaf scan time")
	}

	leaf, err := preflight.ReadIndexUsage(t.Context(), f.pool, f.indexOID(t, "events_2026_k_idx"))
	require.NoError(t, err)
	assert.EqualValues(t, 3, leaf.IdxScans, "a leaf index reports its own scans")
	assert.False(t, leaf.Partitioned)
	assert.Zero(t, leaf.Partitions)
}

// A partitioned index on a table with no partitions has nothing to scan:
// its count is a true zero over zero leaves, not a missing row read as 0.
func TestReadIndexUsageOfAPartitionedIndexWithNoPartitions(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			ts timestamptz NOT NULL,
			k int NOT NULL
		) PARTITION BY RANGE (ts);
		CREATE INDEX events_k_idx ON %[1]s.events (k)`, f.schema))

	u, err := preflight.ReadIndexUsage(t.Context(), f.pool, f.indexOID(t, "events_k_idx"))
	require.NoError(t, err)
	assert.True(t, u.Partitioned)
	assert.Zero(t, u.Partitions)
	assert.Zero(t, u.IdxScans)
}

// An OID that names no index is refused, not reported as an unused index.
func TestReadIndexUsageRefusesAnOIDThatIsNotAnIndex(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id int PRIMARY KEY
		)`, f.schema))

	_, err := preflight.ReadIndexUsage(t.Context(), f.pool, f.indexOID(t, "orders"))
	require.Error(t, err)
}

// A role whose saved settings turn track_counts off scans an index without
// being counted, so the reading session's own track_counts does not prove
// the count complete; the read reports the saved setting.
func TestReadIndexUsageSeesTrackCountsTurnedOffForARole(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id int PRIMARY KEY
		)`, f.schema))
	var database string
	require.NoError(t, f.conn.QueryRow(t.Context(), "SELECT current_database()").Scan(&database))
	// Roles are cluster-wide; the setting is scoped to this test's own
	// database so no other test's read sees it.
	role := pgx.Identifier{f.schema + "_untracked"}.Sanitize()
	f.exec(t, "CREATE ROLE "+role)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), "DROP ROLE "+role)
		assert.NoError(t, err)
	})
	pkey := f.indexOID(t, "orders_pkey")

	f.exec(t, fmt.Sprintf(`ALTER ROLE %s IN DATABASE %s SET track_counts = on`, role, pgx.Identifier{database}.Sanitize()))
	u, err := preflight.ReadIndexUsage(t.Context(), f.pool, pkey)
	require.NoError(t, err)
	assert.False(t, u.TrackCountsOffBySetting, "a saved setting that leaves track_counts on")

	f.exec(t, fmt.Sprintf(`ALTER ROLE %s IN DATABASE %s SET track_counts = off`, role, pgx.Identifier{database}.Sanitize()))
	u, err = preflight.ReadIndexUsage(t.Context(), f.pool, pkey)
	require.NoError(t, err)
	assert.True(t, u.TrackCounts, "the reading session still counts")
	assert.True(t, u.TrackCountsOffBySetting)
}
