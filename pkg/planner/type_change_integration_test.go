package planner_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
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
	// User types that share a built-in's name, each checking every value,
	// so converting to any of them rewrites the table: text in lower case
	// and quoted in upper case, and the quoted SQL-standard spellings of
	// numeric and varchar, which the grammar never emits for the built-ins.
	for _, domain := range []string{
		"CREATE DOMAIN %s.text AS pg_catalog.text CHECK (VALUE <> '')",
		`CREATE DOMAIN %s."TEXT" AS pg_catalog.text CHECK (VALUE <> '')`,
		`CREATE DOMAIN %s."decimal" AS pg_catalog.numeric CHECK (VALUE > 0)`,
		`CREATE DOMAIN %s."character varying" AS pg_catalog.varchar CHECK (VALUE <> '')`,
	} {
		_, err = pool.Exec(t.Context(), fmt.Sprintf(domain, schema))
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
	// Converting timestamptz to timestamp rewrites the table only when the
	// session's time zone is not UTC.
	_, err = conn.Exec(t.Context(), "SET TimeZone = 'America/New_York'")
	require.NoError(t, err)

	// Each case names its table as %[1]s and, where a type is qualified by
	// the fixture schema, that schema as %[2]s: quoted fixture names,
	// substituted per case.
	cases := []struct {
		name     string
		create   string
		alter    string
		route    planner.Route
		rewrites bool
	}{
		{
			name:   "widen varchar",
			create: "CREATE TABLE %[1]s (c varchar(50))",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE varchar(100)",
			route:  planner.RouteNative,
		},
		{
			name:   "varchar to text",
			create: "CREATE TABLE %[1]s (c varchar(50))",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE text",
			route:  planner.RouteNative,
		},
		{
			name:   "varchar to text named through pg_catalog",
			create: "CREATE TABLE %[1]s (c varchar(50))",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE pg_catalog.text",
			route:  planner.RouteNative,
		},
		{
			name:     "varchar to a user type named text",
			create:   "CREATE TABLE %[1]s (c varchar(50))",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE %[2]s.text",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "varchar to a visible user type quoted as TEXT",
			create:   "CREATE TABLE %[1]s (c varchar(50))",
			alter:    `ALTER TABLE %[1]s ALTER COLUMN c TYPE "TEXT"`,
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "text array to an array of a visible user type quoted as TEXT",
			create:   "CREATE TABLE %[1]s (c text[])",
			alter:    `ALTER TABLE %[1]s ALTER COLUMN c TYPE "TEXT"[]`,
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "varchar array to text",
			create:   "CREATE TABLE %[1]s (c varchar(50)[])",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE text",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "varchar array to a wider varchar",
			create:   "CREATE TABLE %[1]s (c varchar(50)[])",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE varchar(100)",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "widen a varchar array's elements",
			create:   "CREATE TABLE %[1]s (c varchar(50)[])",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE varchar(100)[]",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "varchar array to text array",
			create:   "CREATE TABLE %[1]s (c varchar(50)[])",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE text[]",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "widen a numeric array's precision",
			create:   "CREATE TABLE %[1]s (c numeric(10,2)[])",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE numeric(12,2)[]",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "text array to varchar array",
			create:   "CREATE TABLE %[1]s (c text[])",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE varchar[]",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:   "unchanged varchar array",
			create: "CREATE TABLE %[1]s (c varchar(50)[])",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE varchar(50)[]",
			route:  planner.RouteNative,
		},
		{
			name:   "unchanged varchar array spelled ARRAY",
			create: "CREATE TABLE %[1]s (c varchar(50)[])",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE varchar(50) ARRAY",
			route:  planner.RouteNative,
		},
		{
			// PostgreSQL ignores declared array dimensions.
			name:   "unchanged varchar array with more dimensions",
			create: "CREATE TABLE %[1]s (c varchar(50)[])",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE varchar(50)[][]",
			route:  planner.RouteNative,
		},
		{
			name:     "numeric to a visible user type quoted as decimal",
			create:   "CREATE TABLE %[1]s (c numeric(10,2))",
			alter:    `ALTER TABLE %[1]s ALTER COLUMN c TYPE "decimal"`,
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "varchar to a visible user type quoted as character varying",
			create:   "CREATE TABLE %[1]s (c varchar(50))",
			alter:    `ALTER TABLE %[1]s ALTER COLUMN c TYPE "character varying"`,
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "timestamptz with precision to timestamp",
			create:   "CREATE TABLE %[1]s (c timestamptz(3))",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE timestamp(3)",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "timestamptz array with precision to a timestamp array",
			create:   "CREATE TABLE %[1]s (c timestamptz(3)[])",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE timestamp(3)[]",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:     "timetz with precision to time",
			create:   "CREATE TABLE %[1]s (c timetz(3))",
			alter:    "ALTER TABLE %[1]s ALTER COLUMN c TYPE time(3)",
			route:    planner.RouteCopyAndSwap,
			rewrites: true,
		},
		{
			name:   "unchanged timestamptz with precision",
			create: "CREATE TABLE %[1]s (c timestamptz(3))",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE timestamptz(3)",
			route:  planner.RouteNative,
		},
		{
			name:   "unchanged timestamp with precision",
			create: "CREATE TABLE %[1]s (c timestamp(3))",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE timestamp(3)",
			route:  planner.RouteNative,
		},
		{
			// The server relabels this one; the planner has no array rule
			// beyond an unchanged element and takes the slower safe route.
			name:   "drop a varchar array's element bound",
			create: "CREATE TABLE %[1]s (c varchar(50)[])",
			alter:  "ALTER TABLE %[1]s ALTER COLUMN c TYPE varchar[]",
			route:  planner.RouteCopyAndSwap,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := fmt.Sprintf("t%d", i)
			qualified := pgx.Identifier{schema, table}.Sanitize()
			quotedSchema := pgx.Identifier{schema}.Sanitize()
			_, err := conn.Exec(t.Context(), fmt.Sprintf(tc.create, qualified, quotedSchema))
			require.NoError(t, err)
			_, err = conn.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", qualified))
			require.NoError(t, err)

			live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
			require.NoError(t, err)
			alter := fmt.Sprintf(tc.alter, qualified, quotedSchema)
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

			assert.Equal(t, tc.rewrites, before != after, "server rewrite for %s", alter)
			assert.Equal(t, tc.route, decision.Route, "route for %s (reason %s)", alter, decision.Reason)
			if decision.Route == planner.RouteNative {
				assert.Equal(t, planner.ReasonBinaryCoercible, decision.Reason)
				assert.False(t, before != after, "a native verdict must never rewrite the table")
			}
		})
	}
}
