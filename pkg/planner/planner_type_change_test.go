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
