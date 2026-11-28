package executor_test

import (
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
)

// poolInZone opens a pool on base whose sessions run in zone.
func poolInZone(t *testing.T, base, zone string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	q := u.Query()
	q.Set("TimeZone", zone)
	u.RawQuery = q.Encode()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// The planner routes a change between timestamp and timestamptz native only
// when the session time zone it read was UTC. When the zone is something
// else by the time the change runs, because the database or role default
// changed in between, the executor reads the zone again inside the
// attempt's own transaction and refuses with time-zone-drift before the
// ALTER locks the table, so no value moves by the zone's offset.
func TestExecuteNativeRefusesATimeZoneSwapOutsideUTC(t *testing.T) {
	base := testutil.StartPostgres(t)
	pool := poolInZone(t, base, "America/New_York")
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), `
		CREATE TABLE `+schema+`.events (
			id int PRIMARY KEY,
			seen_at timestamp
		);
		INSERT INTO `+schema+`.events VALUES (1, '2026-11-28 12:00:00')`)
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "events")
	var before int64
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_relation_filenode($1::regclass)::int8", schema+".events").Scan(&before))

	st := mustParse(t, `ALTER TABLE `+schema+`.events ALTER COLUMN seen_at TYPE timestamptz`)
	err = executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())

	require.ErrorIs(t, err, executor.ErrTimeZoneDrift)
	assert.Equal(t, executor.CodeTimeZoneDrift, executor.OutcomeCode(err))
	assert.Contains(t, err.Error(), `changes column "seen_at" from timestamp to timestamptz, and this session reads its values in "America/New_York"; plan again`)

	assert.Equal(t, "timestamp without time zone", columnType(t, pool, schema, "events", "seen_at"), "the change must not run")
	var after int64
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_relation_filenode($1::regclass)::int8", schema+".events").Scan(&after))
	assert.Equal(t, before, after, "the table must keep its storage")
}

// In UTC the same change runs, and PostgreSQL relabels the column in
// place.
func TestExecuteNativeRunsATimeZoneSwapInUTC(t *testing.T) {
	base := testutil.StartPostgres(t)
	pool := poolInZone(t, base, "UTC")
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), `
		CREATE TABLE `+schema+`.events (
			id int PRIMARY KEY,
			seen_at timestamp
		);
		INSERT INTO `+schema+`.events VALUES (1, '2026-11-28 12:00:00')`)
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "events")

	st := mustParse(t, `ALTER TABLE `+schema+`.events ALTER COLUMN seen_at TYPE timestamptz`)
	require.NoError(t, executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy()))

	assert.Equal(t, "timestamp with time zone", columnType(t, pool, schema, "events", "seen_at"))
}
