package preflight_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/preflight"
)

// relationOID returns the OID of schema.name in the fixture's database.
func (f usageFixture) relationOID(t *testing.T, name string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.conn.QueryRow(t.Context(),
		"SELECT to_regclass(quote_ident($1) || '.' || quote_ident($2))::oid", f.schema, name).Scan(&oid))
	return oid
}

// holdLock runs lockSQL in an open transaction on a session of its own,
// named usage-lock-holder, and returns its PID. The lock is held until the
// test ends.
func (f usageFixture) holdLock(t *testing.T, lockSQL string) int32 {
	t.Helper()
	cfg, err := pgx.ParseConfig(f.dbURL)
	require.NoError(t, err)
	cfg.RuntimeParams["application_name"] = "usage-lock-holder"
	holder, err := pgx.ConnectConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := holder.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("close lock holder: %v", err)
		}
	})
	var pid int32
	require.NoError(t, holder.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid))
	_, err = holder.Exec(t.Context(), "BEGIN")
	require.NoError(t, err)
	_, err = holder.Exec(t.Context(), lockSQL)
	require.NoError(t, err, lockSQL)
	return pid
}

func partitionNames(partitions []preflight.PartitionUsage) []string {
	names := make([]string, 0, len(partitions))
	for _, p := range partitions {
		names = append(names, p.Name())
	}
	return names
}

// requirePartitionScanned checks the verdict of a partitioned table whose
// one scan, a sequential scan of partition, is the newest evidence: recent
// and naming that partition on PostgreSQL 16 and later, unknown on 14 and
// 15, which record no scan times.
func requirePartitionScanned(t *testing.T, f usageFixture, u preflight.TableUsage, partition string) {
	t.Helper()
	v := judge(t, u)
	named, ok := v.Partition()
	if f.major < 16 {
		assert.Equal(t, preflight.UsageUnknown, v.Outcome())
		assert.Equal(t, preflight.UsageReasonScanTimesNotRecorded, v.Reason())
		assert.False(t, ok)
		return
	}
	assert.Equal(t, preflight.UsageRecent, v.Outcome())
	assert.Equal(t, preflight.UsageReasonSeqScanInWindow, v.Reason())
	require.True(t, ok, "the verdict names the partition scanned")
	assert.Equal(t, partition, named.Name())
	assert.Equal(t, f.relationOID(t, partition), named.OID())
	partitionScan, recorded := named.LastSeqScan()
	require.True(t, recorded)
	tableScan, recorded := u.LastSeqScan()
	require.True(t, recorded)
	assert.Equal(t, partitionScan, tableScan, "the table's last scan is its partition's")
}

// A partitioned table holds no rows; its partitions do, and the server
// records their scans on them. A query the planner prunes to one partition
// scans only that partition, and that partition decides the table: recent,
// naming it. The partitions that were never scanned do not make it unknown,
// because evidence of use comes first. The default partition is a leaf
// like any other.
func TestReadTableUsageJudgesAPartitionedTableByItsMostRecentlyScannedPartition(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.events (
			id bigint,
			note text
		) PARTITION BY RANGE (id)`, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_2025 PARTITION OF %s.events FOR VALUES FROM (0) TO (100)`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_2026 PARTITION OF %s.events FOR VALUES FROM (100) TO (200)`, f.schema, f.schema))
	// Created last: a range partition created beside a default partition
	// scans the default to check none of its rows belong to the new range.
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_default PARTITION OF %s.events DEFAULT`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`INSERT INTO %s.events VALUES (1, 'a'), (150, 'b'), (500, 'c')`, f.schema))

	f.exec(t, fmt.Sprintf(`SELECT count(*) FROM %s.events WHERE id >= 100 AND id < 200`, f.schema))
	f.publish(t)
	u := f.readOnceSeqScans(t, "events", 1)

	requirePrimaryFacts(t, f, u)
	assert.True(t, u.Partitioned())
	assert.Equal(t, int64(3), u.TuplesInserted())
	partitions := u.Partitions()
	assert.Equal(t, []string{"events_2025", "events_2026", "events_default"}, partitionNames(partitions))
	for _, p := range partitions {
		assert.Equal(t, f.schema, p.Schema())
		assert.Equal(t, f.relationOID(t, p.Name()), p.OID())
		assert.False(t, p.Foreign())
	}
	assert.Equal(t, []int64{0, 1, 0}, []int64{partitions[0].SeqScans(), partitions[1].SeqScans(), partitions[2].SeqScans()})
	requirePartitionScanned(t, f, u, "events_2026")
}

// A partition of a partition is a leaf of the table at any depth: a scan
// of a grandchild is use of the table, and the sub-partitioned partition
// between them, which holds no rows, is not listed.
func TestReadTableUsageFollowsSubPartitions(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.events (
			id bigint,
			region text
		) PARTITION BY RANGE (id)`, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_2025 PARTITION OF %s.events FOR VALUES FROM (0) TO (100)`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.events_2026 PARTITION OF %s.events
			FOR VALUES FROM (100) TO (200)
			PARTITION BY LIST (region)`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_2026_eu PARTITION OF %s.events_2026 FOR VALUES IN ('eu')`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_2026_us PARTITION OF %s.events_2026 FOR VALUES IN ('us')`, f.schema, f.schema))

	f.exec(t, fmt.Sprintf(`SELECT count(*) FROM %s.events_2026_eu`, f.schema))
	f.publish(t)
	u := f.readOnceSeqScans(t, "events", 1)

	assert.Equal(t, []string{"events_2025", "events_2026_eu", "events_2026_us"}, partitionNames(u.Partitions()))
	requirePartitionScanned(t, f, u, "events_2026_eu")
}

// Another session's lock on one partition is a lock on the table, on every
// major, and the holder names the partition it locked; locking a partition
// does not lock its parent, so the partition is the only relation listed.
// The read itself takes no lock on any partition, so even an ACCESS
// EXCLUSIVE holder does not make it wait.
func TestReadTableUsageSeesALockOnOnePartition(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.events (
			id bigint
		) PARTITION BY RANGE (id)`, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_2025 PARTITION OF %s.events FOR VALUES FROM (0) TO (100)`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_2026 PARTITION OF %s.events FOR VALUES FROM (100) TO (200)`, f.schema, f.schema))
	pid := f.holdLock(t, fmt.Sprintf(`LOCK TABLE %s.events_2025 IN ACCESS EXCLUSIVE MODE`, f.schema))

	u := f.read(t, "events")
	assert.Equal(t, []preflight.LockHolder{{
		PID:             pid,
		ApplicationName: "usage-lock-holder",
		Modes:           []string{"AccessExclusiveLock"},
		RelationSchema:  f.schema,
		RelationName:    "events_2025",
		RelationOID:     f.relationOID(t, "events_2025"),
	}}, u.LockHolders())
	v := judge(t, u)
	assert.Equal(t, preflight.UsageRecent, v.Outcome())
	assert.Equal(t, preflight.UsageReasonLockHeld, v.Reason())
}

// A partitioned table with no partition holds no rows, and a query against
// it scans nothing, so the server records no trace of it: unknown on
// PostgreSQL 16 and later, never idle. Only a lock on it shows use, and a
// lock on the table itself is a holder like a lock on a partition.
func TestReadTableUsageOfAPartitionedTableWithNoPartition(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.events (
			id bigint
		) PARTITION BY RANGE (id)`, f.schema))
	f.exec(t, fmt.Sprintf(`SELECT count(*) FROM %s.events`, f.schema))
	f.publish(t)

	u := f.read(t, "events")
	assert.True(t, u.Partitioned())
	assert.Empty(t, u.Partitions())
	assert.Zero(t, u.SeqScans())
	v := judge(t, u)
	assert.Equal(t, preflight.UsageUnknown, v.Outcome())
	if f.major < 16 {
		assert.Equal(t, preflight.UsageReasonScanTimesNotRecorded, v.Reason())
	} else {
		assert.Equal(t, preflight.UsageReasonNoPartitions, v.Reason())
	}

	pid := f.holdLock(t, fmt.Sprintf(`LOCK TABLE %s.events IN ACCESS SHARE MODE`, f.schema))
	u = f.read(t, "events")
	assert.Equal(t, []preflight.LockHolder{{
		PID:             pid,
		ApplicationName: "usage-lock-holder",
		Modes:           []string{"AccessShareLock"},
		RelationSchema:  f.schema,
		RelationName:    "events",
		RelationOID:     u.OID(),
	}}, u.LockHolders())
	v = judge(t, u)
	assert.Equal(t, preflight.UsageRecent, v.Outcome())
	assert.Equal(t, preflight.UsageReasonLockHeld, v.Reason())
}

// A foreign table can be a partition, but its rows live on another server
// and this server keeps no scan statistics for it: the table is unknown,
// naming the foreign partition. A foreign table named on its own is not a
// table this check reads. The wrapper has no handler: defining the tables
// needs none, and nothing here reads through them.
func TestReadTableUsageOfAPartitionedTableWithAForeignPartition(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, `CREATE FOREIGN DATA WRAPPER usage_fdw`)
	f.exec(t, `CREATE SERVER usage_remote FOREIGN DATA WRAPPER usage_fdw`)
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.events (
			id bigint
		) PARTITION BY RANGE (id)`, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_2025 PARTITION OF %s.events FOR VALUES FROM (0) TO (100)`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`
		CREATE FOREIGN TABLE %s.events_archive PARTITION OF %s.events
			FOR VALUES FROM (-100) TO (0)
			SERVER usage_remote`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`
		CREATE FOREIGN TABLE %s.remote_orders (
			id bigint
		) SERVER usage_remote`, f.schema))

	u := f.read(t, "events")
	partitions := u.Partitions()
	require.Equal(t, []string{"events_2025", "events_archive"}, partitionNames(partitions))
	assert.False(t, partitions[0].Foreign())
	assert.True(t, partitions[1].Foreign())
	v := judge(t, u)
	assert.Equal(t, preflight.UsageUnknown, v.Outcome())
	named, ok := v.Partition()
	if f.major < 16 {
		assert.Equal(t, preflight.UsageReasonScanTimesNotRecorded, v.Reason())
		assert.False(t, ok)
	} else {
		assert.Equal(t, preflight.UsageReasonForeignPartition, v.Reason())
		require.True(t, ok)
		assert.Equal(t, "events_archive", named.Name())
	}

	_, err := preflight.ReadTableUsage(t.Context(), f.pool, f.schema, "remote_orders")
	require.ErrorIs(t, err, preflight.ErrNotTable)
}

// Inheritance children are not partitions: each is a table of its own, a
// read through the parent is recorded on the parent, and dropping the
// parent needs CASCADE while a child exists. A lock on a child is not a
// lock on the parent.
func TestReadTableUsageDoesNotFollowInheritanceChildren(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id bigint
		)`, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.orders_2025 () INHERITS (%s.orders)`, f.schema, f.schema))
	f.holdLock(t, fmt.Sprintf(`LOCK TABLE ONLY %s.orders_2025 IN ACCESS SHARE MODE`, f.schema))

	u := f.read(t, "orders")
	assert.False(t, u.Partitioned())
	assert.Empty(t, u.Partitions())
	assert.Empty(t, u.LockHolders())
}
