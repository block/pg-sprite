package planner_test

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A change between timestamp and timestamptz keeps the stored bytes only
// when the session time zone is UTC, and rebuilds every index on the
// column even then. Each case runs on a pool whose sessions use the case's
// zone, classifies the change from the facts the front doors read off that
// pool, runs it, and reads the filenodes of the table and of the column's
// index, when it has one. A native verdict must leave both alone.
func TestTimeZoneSwapVerdictMatchesTheServer(t *testing.T) {
	base := testutil.StartPostgres(t)
	pools := map[string]*pgxpool.Pool{}
	poolIn := func(t *testing.T, zone string) *pgxpool.Pool {
		if pool, ok := pools[zone]; ok {
			return pool
		}
		u, err := url.Parse(base)
		require.NoError(t, err)
		q := u.Query()
		q.Set("TimeZone", zone)
		u.RawQuery = q.Encode()
		pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		pools[zone] = pool
		return pool
	}
	schema := testutil.NewSchema(t, poolIn(t, "UTC"))

	cases := []struct {
		name          string
		zone          string
		create        string
		indexed       bool
		alter         string
		route         planner.Route
		rewrites      bool
		rebuildsIndex bool
	}{
		{
			name:   "timestamp to timestamptz in UTC",
			zone:   "UTC",
			create: "CREATE TABLE %s (c timestamp)",
			alter:  "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz",
			route:  planner.RouteNative,
		},
		{
			name:   "timestamptz to timestamp in UTC",
			zone:   "UTC",
			create: "CREATE TABLE %s (c timestamptz)",
			alter:  "ALTER TABLE %s ALTER COLUMN c TYPE timestamp",
			route:  planner.RouteNative,
		},
		{
			name:   "timestamp to timestamptz in Etc/UTC",
			zone:   "Etc/UTC",
			create: "CREATE TABLE %s (c timestamp)",
			alter:  "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz",
			route:  planner.RouteNative,
		},
		{
			name:   "timestamp(3) to timestamptz in UTC",
			zone:   "UTC",
			create: "CREATE TABLE %s (c timestamp(3))",
			alter:  "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz",
			route:  planner.RouteNative,
		},
		{
			name:   "timestamp to timestamptz(6) in UTC",
			zone:   "UTC",
			create: "CREATE TABLE %s (c timestamp)",
			alter:  "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz(6)",
			route:  planner.RouteNative,
		},
		{
			name:     "timestamp(3) to timestamptz(3) in UTC",
			zone:     "UTC",
			create:   "CREATE TABLE %s (c timestamp(3))",
			alter:    "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz(3)",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:          "indexed timestamp to timestamptz in UTC",
			zone:          "UTC",
			create:        "CREATE TABLE %s (c timestamp)",
			indexed:       true,
			alter:         "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz",
			route:         planner.RouteCopyAndSwap,
			rebuildsIndex: true,
		},
		{
			name:     "timestamp array to timestamptz array in UTC",
			zone:     "UTC",
			create:   "CREATE TABLE %s (c timestamp[])",
			alter:    "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz[]",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "timestamp to timestamptz in America/New_York",
			zone:     "America/New_York",
			create:   "CREATE TABLE %s (c timestamp)",
			alter:    "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			// The server relabels this one too; the planner's UTC set is
			// narrow and takes the slower safe route.
			name:   "timestamp to timestamptz in GMT",
			zone:   "GMT",
			create: "CREATE TABLE %s (c timestamp)",
			alter:  "ALTER TABLE %s ALTER COLUMN c TYPE timestamptz",
			route:  planner.RouteCopyAndSwap,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := poolIn(t, tc.zone)
			table := fmt.Sprintf("z%d", i)
			qualified := pgx.Identifier{schema, table}.Sanitize()
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.create, qualified))
			require.NoError(t, err)
			_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", qualified))
			require.NoError(t, err)
			index := qualified
			if tc.indexed {
				_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE INDEX %s ON %s (c)", pgx.Identifier{table + "_c_idx"}.Sanitize(), qualified))
				require.NoError(t, err)
				index = pgx.Identifier{schema, table + "_c_idx"}.Sanitize()
			}

			live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
			require.NoError(t, err)
			facts := planner.FactsFrom(live)
			facts.SessionTimeZone, err = preflight.SessionTimeZone(t.Context(), pool)
			require.NoError(t, err)
			facts.IndexedColumns, err = preflight.IndexedColumns(t.Context(), pool, schema, table)
			require.NoError(t, err)
			alter := fmt.Sprintf(tc.alter, qualified)
			plan, err := planner.Classify(alter, facts)
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			decision := plan.Decisions[0]

			const filenodes = "SELECT pg_relation_filenode($1::regclass)::int8, pg_relation_filenode($2::regclass)::int8"
			var before, after, indexBefore, indexAfter int64
			require.NoError(t, pool.QueryRow(t.Context(), filenodes, qualified, index).Scan(&before, &indexBefore))
			_, err = pool.Exec(t.Context(), alter)
			require.NoError(t, err)
			require.NoError(t, pool.QueryRow(t.Context(), filenodes, qualified, index).Scan(&after, &indexAfter))

			assert.Equal(t, tc.zone, decision.ReadsInTimeZone)
			assert.Equal(t, tc.rewrites, before != after, "server rewrite for %s", alter)
			if tc.indexed {
				assert.Equal(t, tc.rebuildsIndex, indexBefore != indexAfter, "server index rebuild for %s", alter)
			}
			assert.Equal(t, tc.route, decision.Route, "route for %s (reason %s)", alter, decision.Reason)
			assert.False(t, decision.Unverified)
			if decision.Route == planner.RouteNative {
				assert.Equal(t, planner.ReasonBinaryCoercible, decision.Reason)
				assert.False(t, before != after, "a native verdict must never rewrite the table")
				assert.Equal(t, indexBefore, indexAfter, "a native verdict must never rebuild an index")
			}
		})
	}
}

// IndexedColumns names a column an index reads as a key, in an
// expression, or in a predicate, on the table or on a partition below it,
// and leaves out a column no index reads.
func TestIndexedColumnsReadsKeysExpressionsPredicatesAndPartitions(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	q := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	for _, stmt := range []string{
		"CREATE TABLE %[1]s (k int, e text, p bool, part_only timestamp, free timestamp) PARTITION BY RANGE (k)",
		"CREATE TABLE %[2]s PARTITION OF %[1]s FOR VALUES FROM (0) TO (10)",
		"CREATE INDEX ON %[1]s (k)",
		"CREATE INDEX ON %[1]s (lower(e))",
		"CREATE INDEX ON %[2]s (k) WHERE p",
		"CREATE INDEX ON %[2]s (part_only)",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(stmt, q("parent"), q("child")))
		require.NoError(t, err)
	}
	indexed, err := preflight.IndexedColumns(t.Context(), pool, schema, "parent")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"k": true, "e": true, "p": true, "part_only": true}, indexed)

	zone, err := preflight.SessionTimeZone(t.Context(), pool)
	require.NoError(t, err)
	assert.NotEmpty(t, zone)
}
