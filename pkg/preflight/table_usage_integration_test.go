package preflight_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// usageWindow is the window the integration tests judge against: a day,
// the kind of window a drop safeguard would ask about.
const usageWindow = 24 * time.Hour

// statsVisible bounds how long a test waits for a finished scan to show in
// the server's statistics. PostgreSQL 15 and later publish on demand
// (pg_stat_force_next_flush); PostgreSQL 14's statistics collector
// receives counts asynchronously.
const statsVisible = 10 * time.Second

// usageFixture is a database of the test's own — statistics resets are
// per database, so one test's reset cannot reach another's — with a
// throwaway schema, a pool from pkg/dbconn, and a dedicated connection
// whose scans the test publishes.
type usageFixture struct {
	dbURL  string
	pool   *pgxpool.Pool
	conn   *pgxpool.Conn
	schema string
	major  int
}

func newUsageFixture(t *testing.T, serverURL string) usageFixture {
	t.Helper()
	dbURL := testutil.NewDatabase(t, serverURL)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: dbURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	major, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)
	return usageFixture{dbURL: dbURL, pool: pool, conn: conn, schema: testutil.NewSchema(t, pool), major: major}
}

// exec runs sql on the fixture's dedicated connection.
func (f usageFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.conn.Exec(t.Context(), sql)
	require.NoError(t, err, sql)
}

// publish asks the server to publish the dedicated connection's pending
// statistics now, on the versions that can.
func (f usageFixture) publish(t *testing.T) {
	t.Helper()
	if f.major < 15 {
		return
	}
	f.exec(t, "SELECT pg_stat_force_next_flush()")
}

func (f usageFixture) read(t *testing.T, table string) preflight.TableUsage {
	t.Helper()
	u, err := preflight.ReadTableUsage(t.Context(), f.pool, f.schema, table)
	require.NoError(t, err)
	return u
}

// readOnceSeqScans reads the table's usage once its sequential scan count
// has reached want. On PostgreSQL 14 a backend sends its counts to the
// collector only at the end of a transaction at least half a second after
// its last send, and never while idle, so each poll first ends another
// transaction on the dedicated connection.
func (f usageFixture) readOnceSeqScans(t *testing.T, table string, want int64) preflight.TableUsage {
	t.Helper()
	var u preflight.TableUsage
	var readErr error
	require.Eventually(t, func() bool {
		if f.major < 15 {
			if _, readErr = f.conn.Exec(t.Context(), "SELECT 1"); readErr != nil {
				return true
			}
		}
		u, readErr = preflight.ReadTableUsage(t.Context(), f.pool, f.schema, table)
		return readErr != nil || u.SeqScans() == want
	}, statsVisible, 100*time.Millisecond, "the server must publish %d sequential scans of %s", want, table)
	require.NoError(t, readErr)
	return u
}

func judge(t *testing.T, u preflight.TableUsage) preflight.UsageVerdict {
	t.Helper()
	v, err := u.Judge(usageWindow)
	require.NoError(t, err)
	return v
}

// The facts every read carries describe the server the pool is on: a
// primary of the selected major, counting, read at the server's now().
func requirePrimaryFacts(t *testing.T, f usageFixture, u preflight.TableUsage) {
	t.Helper()
	assert.Equal(t, f.major, u.ServerVersionNum()/10000)
	assert.Equal(t, f.major >= 16, u.ScanTimesRecorded())
	assert.False(t, u.InRecovery())
	assert.True(t, u.TrackCounts())
	assert.WithinDuration(t, time.Now(), u.ReadAt(), time.Minute, "the read's time is the server's now()")
}

// A table scanned a moment ago is recent on PostgreSQL 16 and later, with
// the scan's time recorded; on 14 and 15, which record no scan times, the
// same table is unknown. The write counters are read as evidence only.
func TestReadTableUsageJudgesAJustScannedTable(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	// No index: building one scans the table, and the test's own scan is
	// to be the only one. An INSERT scans nothing.
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint,
			note text
		)`, f.schema))
	f.exec(t, fmt.Sprintf(`INSERT INTO %s.orders_archive VALUES (1, 'a'), (2, 'b'), (3, 'c')`, f.schema))

	var scannedAfter time.Time
	require.NoError(t, f.conn.QueryRow(t.Context(), "SELECT clock_timestamp()").Scan(&scannedAfter))
	f.exec(t, fmt.Sprintf(`SELECT count(*) FROM %s.orders_archive`, f.schema))
	f.publish(t)
	u := f.readOnceSeqScans(t, "orders_archive", 1)

	requirePrimaryFacts(t, f, u)
	assert.Equal(t, int64(3), u.TuplesInserted())
	assert.Zero(t, u.TuplesUpdated())
	assert.Zero(t, u.TuplesDeleted())
	assert.Empty(t, u.LockHolders())
	v := judge(t, u)
	lastSeqScan, recorded := u.LastSeqScan()
	if f.major < 16 {
		assert.False(t, recorded)
		assert.Equal(t, preflight.UsageUnknown, v.Outcome())
		assert.Equal(t, preflight.UsageReasonScanTimesNotRecorded, v.Reason())
		return
	}
	require.True(t, recorded)
	assert.False(t, lastSeqScan.Before(scannedAfter), "the recorded scan is the one the test ran")
	assert.False(t, lastSeqScan.After(u.ReadAt()))
	assert.Equal(t, preflight.UsageRecent, v.Outcome())
	assert.Equal(t, preflight.UsageReasonSeqScanInWindow, v.Reason())
}

// An index scan is recorded against the table through its index. The
// single-table reset clears the table's sequential scan time first, so the
// index scan is the only scan time left to decide on: it is recent even
// though the reset is inside the window too.
func TestReadTableUsageRecordsAnIndexScan(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	if f.major < 16 {
		t.Skipf("PostgreSQL %d records no scan times", f.major)
	}
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint PRIMARY KEY,
			note text
		)`, f.schema))
	f.exec(t, fmt.Sprintf(`INSERT INTO %s.orders_archive VALUES (1, 'a'), (2, 'b')`, f.schema))
	// The index build's scan is published before the reset, so the reset
	// clears it rather than a later publication restoring it.
	f.publish(t)
	f.exec(t, fmt.Sprintf(`SELECT pg_stat_reset_single_table_counters('%s.orders_archive'::regclass)`, f.schema))
	f.exec(t, "SET enable_seqscan = off")
	f.exec(t, fmt.Sprintf(`SELECT note FROM %s.orders_archive WHERE id = 2`, f.schema))
	f.publish(t)

	u := f.read(t, "orders_archive")
	assert.Equal(t, int64(1), u.IdxScans())
	_, seqRecorded := u.LastSeqScan()
	assert.False(t, seqRecorded)
	_, idxRecorded := u.LastIdxScan()
	assert.True(t, idxRecorded)
	v := judge(t, u)
	assert.Equal(t, preflight.UsageRecent, v.Outcome())
	assert.Equal(t, preflight.UsageReasonIndexScanInWindow, v.Reason())
}

// A session with an open transaction holding a lock on the table is use on
// every major, and the holder is named in the evidence. Once it ends, the
// table has no holder.
func TestReadTableUsageSeesALockHolder(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint
		)`, f.schema))

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
	var holderPID int32
	require.NoError(t, holder.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&holderPID))
	tx, err := holder.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s.orders_archive IN ACCESS SHARE MODE`, f.schema))
	require.NoError(t, err)

	u := f.read(t, "orders_archive")
	requirePrimaryFacts(t, f, u)
	assert.Equal(t, []preflight.LockHolder{{
		PID:             holderPID,
		ApplicationName: "usage-lock-holder",
		Modes:           []string{"AccessShareLock"},
		RelationSchema:  f.schema,
		RelationName:    "orders_archive",
		RelationOID:     u.OID(),
	}}, u.LockHolders())
	v := judge(t, u)
	assert.Equal(t, preflight.UsageRecent, v.Outcome())
	assert.Equal(t, preflight.UsageReasonLockHeld, v.Reason())

	require.NoError(t, tx.Rollback(t.Context()))
	assert.Empty(t, f.read(t, "orders_archive").LockHolders())
}

// A prepared transaction keeps its locks with no backend: it is a holder
// all the same, with no PID and no application name.
func TestReadTableUsageSeesAPreparedTransactionHolder(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgresWithSettings(t, "max_prepared_transactions=2"))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint
		)`, f.schema))
	gid := "usage_" + f.schema
	f.exec(t, "BEGIN")
	f.exec(t, fmt.Sprintf(`INSERT INTO %s.orders_archive VALUES (1)`, f.schema))
	f.exec(t, fmt.Sprintf(`PREPARE TRANSACTION '%s'`, gid))
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf(`ROLLBACK PREPARED '%s'`, gid)); err != nil {
			t.Logf("roll back prepared transaction %s: %v", gid, err)
		}
	})

	u := f.read(t, "orders_archive")
	assert.Equal(t, []preflight.LockHolder{{
		PID:             0,
		ApplicationName: "",
		Modes:           []string{"RowExclusiveLock"},
		RelationSchema:  f.schema,
		RelationName:    "orders_archive",
		RelationOID:     u.OID(),
	}}, u.LockHolders())
	v := judge(t, u)
	assert.Equal(t, preflight.UsageRecent, v.Outcome())
	assert.Equal(t, preflight.UsageReasonLockHeld, v.Reason())
}

// After pg_stat_reset() the table's scans are gone and the reset is inside
// the window, so "no scan recorded" proves nothing: unknown, never idle.
func TestReadTableUsageAfterADatabaseResetIsNotIdle(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint
		)`, f.schema))
	f.exec(t, fmt.Sprintf(`SELECT count(*) FROM %s.orders_archive`, f.schema))
	f.publish(t)
	f.readOnceSeqScans(t, "orders_archive", 1)
	f.exec(t, "SELECT pg_stat_reset()")

	u := f.readOnceSeqScans(t, "orders_archive", 0)
	requireResetInsideWindow(t, f, u)
}

// A single-table reset moves the database's reset time too, so it makes
// the answer unknown exactly as a database reset does.
func TestReadTableUsageAfterASingleTableResetIsNotIdle(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint
		)`, f.schema))
	f.exec(t, fmt.Sprintf(`SELECT count(*) FROM %s.orders_archive`, f.schema))
	f.publish(t)
	f.readOnceSeqScans(t, "orders_archive", 1)
	f.exec(t, fmt.Sprintf(`SELECT pg_stat_reset_single_table_counters('%s.orders_archive'::regclass)`, f.schema))

	u := f.readOnceSeqScans(t, "orders_archive", 0)
	requireResetInsideWindow(t, f, u)
}

func requireResetInsideWindow(t *testing.T, f usageFixture, u preflight.TableUsage) {
	t.Helper()
	resetAt, reset := u.StatsReset()
	require.True(t, reset, "the reset is recorded")
	assert.WithinDuration(t, u.ReadAt(), resetAt, time.Minute)
	v := judge(t, u)
	assert.Equal(t, preflight.UsageUnknown, v.Outcome())
	if f.major < 16 {
		assert.Equal(t, preflight.UsageReasonScanTimesNotRecorded, v.Reason())
		return
	}
	_, scanned := u.LastSeqScan()
	assert.False(t, scanned, "the reset cleared the scan time")
	assert.Equal(t, preflight.UsageReasonStatsResetInWindow, v.Reason())
}

// A database that has never had its statistics reset reports no reset
// time, so a table with no recorded scan cannot be shown to have gone
// unscanned for the window: unknown. PostgreSQL 14 gives a new database a
// reset time but records no scan times.
func TestReadTableUsageNeverScannedWithoutResetTimeIsUnknown(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint
		)`, f.schema))

	u := f.read(t, "orders_archive")
	assert.Zero(t, u.SeqScans())
	v := judge(t, u)
	assert.Equal(t, preflight.UsageUnknown, v.Outcome())
	if f.major < 16 {
		assert.Equal(t, preflight.UsageReasonScanTimesNotRecorded, v.Reason())
		return
	}
	_, reset := u.StatsReset()
	assert.False(t, reset)
	assert.Equal(t, preflight.UsageReasonStatsStartUnknown, v.Reason())
}

// Only an ordinary or a partitioned table is read: a view is not a table,
// and a missing name is not found. A single partition named directly is
// read as the ordinary table it is.
func TestReadTableUsageRefusesWhatIsNotATable(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.events (
			id bigint
		) PARTITION BY RANGE (id)`, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_1 PARTITION OF %s.events FOR VALUES FROM (0) TO (100)`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE VIEW %s.events_view AS SELECT id FROM %s.events`, f.schema, f.schema))

	_, err := preflight.ReadTableUsage(t.Context(), f.pool, f.schema, "events_view")
	require.ErrorIs(t, err, preflight.ErrNotTable)
	_, err = preflight.ReadTableUsage(t.Context(), f.pool, f.schema, "absent")
	require.ErrorIs(t, err, preflight.ErrTableNotFound)

	u, err := preflight.ReadTableUsage(t.Context(), f.pool, f.schema, "events_1")
	require.NoError(t, err)
	assert.Equal(t, "events_1", u.Table())
	assert.False(t, u.Partitioned())
	assert.Empty(t, u.Partitions())
}

// The read resolves to the real catalog whatever the session's search_path
// says: a lookalike pg_is_in_recovery() claiming a standby and an empty
// lookalike pg_locks ahead of pg_catalog change neither answer, and an
// empty lookalike pg_inherits hides no partition.
func TestReadTableUsageIgnoresTheSessionSearchPath(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	f := newUsageFixture(t, serverURL)
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint
		)`, f.schema))
	f.exec(t, fmt.Sprintf(`
		CREATE FUNCTION %s.pg_is_in_recovery() RETURNS boolean LANGUAGE sql
		AS 'SELECT true'`, f.schema))
	f.exec(t, fmt.Sprintf(`
		CREATE VIEW %s.pg_locks AS
		SELECT * FROM pg_catalog.pg_locks WHERE false`, f.schema))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.events (
			id bigint
		) PARTITION BY RANGE (id)`, f.schema))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s.events_1 PARTITION OF %s.events FOR VALUES FROM (0) TO (100)`, f.schema, f.schema))
	f.exec(t, fmt.Sprintf(`
		CREATE VIEW %s.pg_inherits AS
		SELECT * FROM pg_catalog.pg_inherits WHERE false`, f.schema))
	shadowing := testutil.NewCatalogShadowingPool(t, f.dbURL, f.schema)
	var lookalike bool
	require.NoError(t, shadowing.QueryRow(t.Context(), "SELECT pg_is_in_recovery()").Scan(&lookalike))
	require.True(t, lookalike, "the lookalike answers the unqualified call on this path")

	f.exec(t, "BEGIN")
	f.exec(t, fmt.Sprintf(`LOCK TABLE %s.orders_archive IN ACCESS SHARE MODE`, f.schema))
	t.Cleanup(func() {
		if _, err := f.conn.Exec(context.WithoutCancel(t.Context()), "ROLLBACK"); err != nil {
			t.Logf("roll back lock holder: %v", err)
		}
	})

	u, err := preflight.ReadTableUsage(t.Context(), shadowing, f.schema, "orders_archive")
	require.NoError(t, err)
	assert.False(t, u.InRecovery())
	require.Len(t, u.LockHolders(), 1)
	assert.Equal(t, []string{"AccessShareLock"}, u.LockHolders()[0].Modes)

	partitioned, err := preflight.ReadTableUsage(t.Context(), shadowing, f.schema, "events")
	require.NoError(t, err)
	assert.Equal(t, []string{"events_1"}, partitionNames(partitioned.Partitions()))
}
