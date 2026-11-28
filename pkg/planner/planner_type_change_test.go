package planner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
)

// A type change is a catalog relabel only when both the live type and the
// target type are fully understood. Each case here is a change PostgreSQL
// rewrites the table for, or fails to convert, even though the part of the
// type the planner models (family plus modifiers) looks identical on both
// sides; every one must take the rewrite route, never the relabel route.
func TestClassifyTypeChangeRewritesWhatTheModelCannotSee(t *testing.T) {
	cases := []struct {
		name     string
		liveType string
		sql      string
	}{
		{
			// timestamptz(3) -> timestamp(3) rewrites the table under
			// ACCESS EXCLUSIVE: the live type's "with time zone" suffix is
			// what distinguishes the two.
			name:     "timestamptz to timestamp with the same precision",
			liveType: "timestamp(3) with time zone",
			sql:      "ALTER TABLE t ALTER COLUMN c TYPE timestamp(3)",
		},
		{
			name:     "timetz to time with the same precision",
			liveType: "time(3) with time zone",
			sql:      "ALTER TABLE t ALTER COLUMN c TYPE time(3)",
		},
		{
			// A COLLATE clause changes the column's collation, which
			// rebuilds every index on it.
			name:     "target with a collation",
			liveType: "character varying(50)",
			sql:      `ALTER TABLE t ALTER COLUMN c TYPE text COLLATE "C"`,
		},
		{
			name:     "live type with trailing text after the modifiers",
			liveType: "character varying(50) extra",
			sql:      "ALTER TABLE t ALTER COLUMN c TYPE varchar(100)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planner.Classify(tc.sql, planner.Facts{ColumnTypes: map[string]string{"c": tc.liveType}})
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			assert.Equal(t, planner.RouteCopyAndSwap, plan.Decisions[0].Route)
			assert.Equal(t, planner.ReasonTypeRewrite, plan.Decisions[0].Reason)
			assert.False(t, plan.Decisions[0].Unverified, "the live type was known; this is a proven rewrite route")
		})
	}
}

// A column with a collation of its own that is retyped without a COLLATE
// clause takes the new type's default collation, so every index on it is
// rebuilt under ACCESS EXCLUSIVE and its sort order changes. The type part
// of the change is a textbook relabel (varchar widen); the collation makes
// it a rewrite, and the live fact is known, so the route is proven.
func TestClassifyTypeChangeRewritesACollatedLiveColumn(t *testing.T) {
	facts := planner.Facts{
		ColumnTypes:      map[string]string{"c": "character varying(50)"},
		ColumnCollations: map[string]string{"c": `pg_catalog."C"`},
	}
	plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN c TYPE varchar(100)", facts)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, plan.Decisions[0].Route)
	assert.Equal(t, planner.ReasonTypeRewrite, plan.Decisions[0].Reason)
	assert.False(t, plan.Decisions[0].Unverified, "the live collation was known; this is a proven rewrite route")
}

// A COLLATE clause that names the live column's own collation keeps it, so
// the type rules decide: a varchar widen stays a relabel and reuses the
// column's indexes. A clause naming another collation, or none, changes the
// collation and stays a rewrite. An unqualified name keeps only a
// pg_catalog collation, since the engine's sessions search pg_catalog
// first and any other schema depends on the search path. A name with a
// database part is never read as kept: joined, its first parts would match
// a schema whose name holds a dot.
func TestClassifyTypeChangeKeepsTheRelabelWhenTheCollationStays(t *testing.T) {
	for _, tc := range []struct {
		name, live, sql string
		route           planner.Route
	}{
		{name: "unqualified, built-in", live: `pg_catalog."C"`, sql: `ALTER TABLE t ALTER COLUMN c TYPE varchar(100) COLLATE "C"`, route: planner.RouteNative},
		{name: "qualified, built-in", live: `pg_catalog."C"`, sql: `ALTER TABLE t ALTER COLUMN c TYPE varchar(100) COLLATE pg_catalog."C"`, route: planner.RouteNative},
		{name: "qualified, schema-local", live: `app.names`, sql: `ALTER TABLE t ALTER COLUMN c TYPE varchar(100) COLLATE app.names`, route: planner.RouteNative},
		{name: "unqualified, schema-local", live: `app.names`, sql: `ALTER TABLE t ALTER COLUMN c TYPE varchar(100) COLLATE names`, route: planner.RouteCopyAndSwap},
		{name: "another collation", live: `pg_catalog."C"`, sql: `ALTER TABLE t ALTER COLUMN c TYPE varchar(100) COLLATE "POSIX"`, route: planner.RouteCopyAndSwap},
		{name: "another schema", live: `pg_catalog."C"`, sql: `ALTER TABLE t ALTER COLUMN c TYPE varchar(100) COLLATE app."C"`, route: planner.RouteCopyAndSwap},
		{name: "kept, narrowed", live: `pg_catalog."C"`, sql: `ALTER TABLE t ALTER COLUMN c TYPE varchar(20) COLLATE "C"`, route: planner.RouteCopyAndSwap},
		{name: "database-qualified", live: `"a.b".x`, sql: `ALTER TABLE t ALTER COLUMN c TYPE varchar(100) COLLATE a.b.x`, route: planner.RouteCopyAndSwap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := planner.Facts{
				ColumnTypes:      map[string]string{"c": "character varying(50)"},
				ColumnCollations: map[string]string{"c": tc.live},
			}
			plan, err := planner.Classify(tc.sql, facts)
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			assert.Equal(t, tc.route, plan.Decisions[0].Route)
			assert.False(t, plan.Decisions[0].Unverified)
		})
	}

	uncollated := planner.Facts{ColumnTypes: map[string]string{"c": "character varying(50)"}}
	plan, err := planner.Classify(`ALTER TABLE t ALTER COLUMN c TYPE varchar(100) COLLATE "C"`, uncollated)
	require.NoError(t, err)
	assert.Equal(t, planner.RouteCopyAndSwap, plan.Decisions[0].Route, "a clause on a column with the default collation changes it")
}

// The same widen on a column with the default collation stays a relabel:
// the collation fact must only ever take the route away from native for
// the column it names.
func TestClassifyTypeChangeKeepsTheRelabelForAnUncollatedColumn(t *testing.T) {
	facts := planner.Facts{
		ColumnTypes:      map[string]string{"c": "character varying(50)", "other": "text"},
		ColumnCollations: map[string]string{"other": `pg_catalog."C"`},
	}
	plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN c TYPE varchar(100)", facts)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.RouteNative, plan.Decisions[0].Route)
	assert.Equal(t, planner.ReasonBinaryCoercible, plan.Decisions[0].Reason)
}

// A pg_catalog qualifier names the built-in type, so it must not defeat the
// relabel proof: the SQL-standard spelling and the explicit qualifier are the
// same varchar widen as the bare form.
func TestClassifyTypeChangeAcceptsPgCatalogQualifiedTarget(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE t ALTER COLUMN c TYPE pg_catalog.varchar(100)",
		"ALTER TABLE t ALTER COLUMN c TYPE character varying(100)",
	} {
		plan, err := planner.Classify(sql, planner.Facts{ColumnTypes: map[string]string{"c": "character varying(50)"}})
		require.NoError(t, err)
		require.Len(t, plan.Decisions, 1)
		assert.Equal(t, planner.RouteNative, plan.Decisions[0].Route, sql)
		assert.Equal(t, planner.ReasonBinaryCoercible, plan.Decisions[0].Reason, sql)
	}
}

// The temporal precision, varbit, and cidr rules read the live type as
// format_type renders it, with the modifiers inside a multi-word name, and
// match the target as the grammar spells it. Each relabel keeps every
// stored value; each rewrite rounds, truncates, or masks one.
func TestClassifyTypeChangeRelabelsTemporalPrecisionVarbitAndCidr(t *testing.T) {
	for name, tc := range map[string]struct {
		liveType, target string
		relabel          bool
	}{
		"raise timestamp precision":          {"timestamp(3) without time zone", "timestamp(6)", true},
		"drop timestamp precision":           {"timestamp(3) without time zone", "timestamp", true},
		"timestamp to the maximum precision": {"timestamp without time zone", "timestamp(6)", true},
		"timestamp above the maximum":        {"timestamp(3) without time zone", "timestamp(7)", true},
		"lower timestamp precision":          {"timestamp(6) without time zone", "timestamp(3)", false},
		"give timestamp a lower precision":   {"timestamp without time zone", "timestamp(3)", false},
		"raise timestamptz precision":        {"timestamp(3) with time zone", "timestamptz(6)", true},
		"timestamp to timestamptz":           {"timestamp(3) without time zone", "timestamptz(3)", false},
		"raise time precision":               {"time(2) without time zone", "time(5)", true},
		"drop timetz precision":              {"time(2) with time zone", "timetz", true},
		"lower timetz precision":             {"time(5) with time zone", "timetz(2)", false},
		"raise a timestamp array precision":  {"timestamp(3) without time zone[]", "timestamp(6)[]", false},
		"widen varbit":                       {"bit varying(8)", "varbit(16)", true},
		"widen bit varying":                  {"bit varying(8)", "bit varying(16)", true},
		"drop varbit length":                 {"bit varying(8)", "varbit", true},
		"narrow varbit":                      {"bit varying(16)", "varbit(8)", false},
		"widen fixed bit":                    {"bit(8)", "bit(16)", false},
		"cidr to inet":                       {"cidr", "inet", true},
		"inet to cidr":                       {"inet", "cidr", false},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN c TYPE "+tc.target, planner.Facts{ColumnTypes: map[string]string{"c": tc.liveType}})
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			if tc.relabel {
				assert.Equal(t, planner.RouteNative, plan.Decisions[0].Route)
				assert.Equal(t, planner.ReasonBinaryCoercible, plan.Decisions[0].Reason)
				return
			}
			assert.Equal(t, planner.RouteCopyAndSwap, plan.Decisions[0].Route)
			assert.Equal(t, planner.ReasonTypeRewrite, plan.Decisions[0].Reason)
		})
	}
}

// A change between timestamp and timestamptz relabels only when the
// session time zone is UTC and no index reads the column. A zone or an
// index that was not read, as in an offline lint, fails closed to the
// rewrite route and says the change may be cheaper with live facts. Every
// decision names the zone it was classified in.
func TestClassifyTimeZoneSwapNeedsTheZoneAndTheIndexes(t *testing.T) {
	unindexed := map[string]bool{"other": true}
	for name, tc := range map[string]struct {
		zone       string
		indexed    map[string]bool
		route      planner.Route
		unverified bool
	}{
		"in UTC with no index":            {"UTC", unindexed, planner.RouteNative, false},
		"in UTC with an index":            {"UTC", map[string]bool{"c": true}, planner.RouteCopyAndSwap, false},
		"in UTC with indexes unread":      {"UTC", nil, planner.RouteCopyAndSwap, true},
		"in another zone":                 {"Europe/Paris", unindexed, planner.RouteCopyAndSwap, false},
		"in another zone, indexes unread": {"Europe/Paris", nil, planner.RouteCopyAndSwap, false},
		"with the zone unread":            {"", unindexed, planner.RouteCopyAndSwap, true},
	} {
		t.Run(name, func(t *testing.T) {
			facts := planner.Facts{
				ColumnTypes:     map[string]string{"c": "timestamp without time zone"},
				SessionTimeZone: tc.zone,
				KeyedColumns:    tc.indexed,
			}
			plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN c TYPE timestamptz", facts)
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			d := plan.Decisions[0]
			assert.Equal(t, tc.route, d.Route)
			assert.Equal(t, tc.unverified, d.Unverified)
			assert.Equal(t, tc.zone, d.ReadsInTimeZone)
		})
	}

	t.Run("a USING expression decides the conversion", func(t *testing.T) {
		facts := planner.Facts{
			ColumnTypes:     map[string]string{"c": "timestamp without time zone"},
			SessionTimeZone: "UTC",
			KeyedColumns:    unindexed,
		}
		plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN c TYPE timestamptz USING c AT TIME ZONE 'UTC'", facts)
		require.NoError(t, err)
		require.Len(t, plan.Decisions, 1)
		assert.Equal(t, planner.RouteCopyAndSwap, plan.Decisions[0].Route)
		assert.Empty(t, plan.Decisions[0].ReadsInTimeZone)
	})
}

// A policy that reads a column changed between timestamp and timestamptz
// sends the operator to expand and contract even where the change relabels
// in place, since the manual path runs in a session whose time zone the
// plan did not read.
func TestClassifyTimeZoneSwapUnderAPolicyExpandsAndContracts(t *testing.T) {
	policies := []string{`"recent_rows" ON "public"."t"`}
	facts := planner.Facts{
		ColumnTypes:           map[string]string{"c": "timestamp without time zone"},
		SessionTimeZone:       "UTC",
		KeyedColumns:          map[string]bool{},
		DependentPolicies:     map[string][]string{"c": policies},
		DropDependentPolicies: map[string][]string{"c": policies},
		OwnDependentPolicies:  map[string][]string{"c": policies},
	}
	plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN c TYPE timestamptz", facts)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.ReasonDependentPolicy, plan.Decisions[0].Reason)
	assert.Equal(t, planner.DependentFixExpandContract, plan.Decisions[0].DependentFix)
}
