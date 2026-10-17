package planner_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A type change the planner routes native as binary-coercible runs as
// written under ACCESS EXCLUSIVE, so the verdict is only safe if the server
// agrees that nothing is rewritten. Each case classifies the change from
// facts introspected off a real column, runs it, and reads the table's
// filenode before and after: a changed filenode is a rewrite. A native
// verdict on a change the server rewrites would run a blocking full-table
// rewrite natively. The reverse, a rewrite route for a change the server
// relabels, is only slower, and the rule set stays narrow on purpose; the
// case that pins it says so.
func TestBinaryCoercibleVerdictMatchesTheServer(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	// User types that share the built-in text's name, one in lower case
	// and one quoted in upper case, each checking every value, so
	// converting to either rewrites the table.
	for _, domain := range []string{"text", `"TEXT"`} {
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			"CREATE DOMAIN %s.%s AS pg_catalog.text CHECK (VALUE <> '')", schema, domain))
		require.NoError(t, err)
	}
	// The statements run with the schema on the search path, so an
	// unqualified "TEXT" resolves to the user type. pg_catalog is still
	// searched first, so an unqualified text stays the built-in.
	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	_, err = conn.Exec(t.Context(), "SET search_path = "+schema)
	require.NoError(t, err)

	cases := []struct {
		name     string
		from     string
		to       string
		route    planner.Route
		rewrites bool
	}{
		{"widen varchar", "varchar(50)", "varchar(100)", planner.RouteNative, false},
		{"varchar to text", "varchar(50)", "text", planner.RouteNative, false},
		{"varchar to text named through pg_catalog", "varchar(50)", "pg_catalog.text", planner.RouteNative, false},
		{"varchar to a user type named text", "varchar(50)", schema + ".text", planner.RouteCopyAndSwap, true},
		{"varchar to a visible user type quoted as TEXT", "varchar(50)", `"TEXT"`, planner.RouteCopyAndSwap, true},
		{"text array to an array of a visible user type quoted as TEXT", "text[]", `"TEXT"[]`, planner.RouteCopyAndSwap, true},
		{"varchar array to text", "varchar(50)[]", "text", planner.RouteCopyAndSwap, true},
		{"varchar array to a wider varchar", "varchar(50)[]", "varchar(100)", planner.RouteCopyAndSwap, true},
		{"widen a varchar array's elements", "varchar(50)[]", "varchar(100)[]", planner.RouteCopyAndSwap, true},
		{"varchar array to text array", "varchar(50)[]", "text[]", planner.RouteCopyAndSwap, true},
		{"widen a numeric array's precision", "numeric(10,2)[]", "numeric(12,2)[]", planner.RouteCopyAndSwap, true},
		{"text array to varchar array", "text[]", "varchar[]", planner.RouteCopyAndSwap, true},
		{"unchanged varchar array", "varchar(50)[]", "varchar(50)[]", planner.RouteNative, false},
		{"unchanged varchar array spelled ARRAY", "varchar(50)[]", "varchar(50) ARRAY", planner.RouteNative, false},
		// PostgreSQL ignores declared array dimensions.
		{"unchanged varchar array with more dimensions", "varchar(50)[]", "varchar(50)[][]", planner.RouteNative, false},
		// The server relabels this one; the planner has no array rule
		// beyond an unchanged element and takes the slower safe route.
		{"drop a varchar array's element bound", "varchar(50)[]", "varchar[]", planner.RouteCopyAndSwap, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := fmt.Sprintf("t%d", i)
			qualified := schema + "." + table
			_, err := conn.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s (c %s)", qualified, tc.from))
			require.NoError(t, err)
			_, err = conn.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", qualified))
			require.NoError(t, err)

			live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
			require.NoError(t, err)
			alter := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN c TYPE %s", qualified, tc.to)
			plan, err := planner.Classify(alter, planner.FactsFrom(live))
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			decision := plan.Decisions[0]

			var before, after int64
			require.NoError(t, conn.QueryRow(t.Context(),
				"SELECT pg_relation_filenode($1::regclass)::int8", qualified).Scan(&before))
			_, err = conn.Exec(t.Context(), alter)
			require.NoError(t, err)
			require.NoError(t, conn.QueryRow(t.Context(),
				"SELECT pg_relation_filenode($1::regclass)::int8", qualified).Scan(&after))

			assert.Equal(t, tc.rewrites, before != after, "server rewrite for %s -> %s", tc.from, tc.to)
			assert.Equal(t, tc.route, decision.Route, "route for %s -> %s (reason %s)", tc.from, tc.to, decision.Reason)
			if decision.Route == planner.RouteNative {
				assert.Equal(t, planner.ReasonBinaryCoercible, decision.Reason)
				assert.False(t, before != after, "a native verdict must never rewrite the table")
			}
		})
	}
}
