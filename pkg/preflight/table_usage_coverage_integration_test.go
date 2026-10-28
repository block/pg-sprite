package preflight_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/preflight"
)

// A pg_stat_reset() that lands after the read has taken the database's
// reset time, but before it reads the table's statistics, must not pair
// the old reset time with the scan times the reset cleared: the read sees
// one snapshot, so it reports the scan it saw before the reset. Without
// one snapshot the table would read as never scanned since an old reset,
// which is the evidence of an idle table.
func TestReadTableUsageSeesOneStatisticsSnapshot(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	if f.major < 16 {
		t.Skipf("PostgreSQL %d records no scan times", f.major)
	}
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint
		)`, f.schema))
	f.exec(t, fmt.Sprintf(`SELECT count(*) FROM %s.orders_archive`, f.schema))
	f.publish(t)
	before := f.readOnceSeqScans(t, "orders_archive", 1)
	resetBefore, hadReset := before.StatsReset()

	// The hook runs on the reading connection's goroutine, as the read
	// sends its table statistics query, and resets from the fixture's own
	// connection.
	reader := hookedPool(t, f.dbURL, "WITH RECURSIVE tree", func() {
		f.exec(t, "SELECT pg_stat_reset()")
	})
	u, err := preflight.ReadTableUsage(t.Context(), reader, f.schema, "orders_archive")
	require.NoError(t, err)

	assert.Equal(t, int64(1), u.SeqScans(), "the scan the read saw before the reset")
	_, scanned := u.LastSeqScan()
	assert.True(t, scanned, "the scan time the read saw before the reset")
	resetAt, reset := u.StatsReset()
	assert.Equal(t, hadReset, reset)
	assert.Equal(t, resetBefore, resetAt, "the reset time from the same snapshot as the scan")
	v := judge(t, u)
	assert.Equal(t, preflight.UsageRecent, v.Outcome())
	assert.Equal(t, preflight.UsageReasonSeqScanInWindow, v.Reason())

	// The reset did happen: a read after it sees it and no scan.
	after := f.readOnceSeqScans(t, "orders_archive", 0)
	resetAfter, reset := after.StatsReset()
	require.True(t, reset)
	assert.True(t, !hadReset || resetAfter.After(resetBefore), "the hook's reset moved the reset time")
}

// A role whose saved settings turn track_counts off in this database
// scans without being counted, so the reading session's own track_counts
// does not prove the table's statistics are complete: the answer is
// unknown. A saved setting that leaves it on changes nothing.
func TestReadTableUsageSeesTrackCountsTurnedOffForARole(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %s.orders_archive (
			id bigint
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

	f.exec(t, fmt.Sprintf(`ALTER ROLE %s IN DATABASE %s SET track_counts = on`, role, pgx.Identifier{database}.Sanitize()))
	u := f.read(t, "orders_archive")
	assert.True(t, u.TrackCounts())
	assert.False(t, u.TrackCountsOffBySetting(), "a saved setting that leaves track_counts on")

	f.exec(t, fmt.Sprintf(`ALTER ROLE %s IN DATABASE %s SET track_counts = off`, role, pgx.Identifier{database}.Sanitize()))
	u = f.read(t, "orders_archive")
	assert.True(t, u.TrackCounts(), "the reading session still counts")
	assert.True(t, u.TrackCountsOffBySetting())
	v := judge(t, u)
	assert.Equal(t, preflight.UsageUnknown, v.Outcome())
	if f.major < 16 {
		assert.Equal(t, preflight.UsageReasonScanTimesNotRecorded, v.Reason())
		return
	}
	assert.Equal(t, preflight.UsageReasonCountsNotTrackedBySetting, v.Reason())
}

// hookedPool is a test-only pool, connected straight to dbURL, whose
// connections run hook once, as the first statement containing marker
// begins. Production code connects through dbconn.
func hookedPool(t *testing.T, dbURL, marker string, hook func()) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(dbURL)
	require.NoError(t, err)
	pc.ConnConfig.Tracer = &statementHook{marker: marker, hook: hook}
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// statementHook is a pgx query tracer that runs its hook once, before the
// first statement whose text contains marker is sent.
type statementHook struct {
	marker string
	hook   func()
	once   sync.Once
}

func (h *statementHook) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, h.marker) {
		h.once.Do(h.hook)
	}
	return ctx
}

func (h *statementHook) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
