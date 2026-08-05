package planner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
)

// facts mirrors a live table whose column types exercise both sides of the
// binary-coercible rules.
var facts = planner.Facts{ColumnTypes: map[string]string{
	"v50":  "character varying(50)",
	"vany": "character varying",
	"num":  "numeric(10,2)",
	"i":    "integer",
	"txt":  "text",
}}

func classifyOne(t *testing.T, sql string) planner.Decision {
	t.Helper()
	plan, err := planner.Classify(sql, facts)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, plan.Decisions[0].Route, plan.Route, "single-decision plan route must match")
	return plan.Decisions[0]
}

// TestClassifyReferenceRows is the golden mapping: one case per row of
// docs/postgres-online-ddl-reference.md. saferSteps is the length of the
// expected safer sequence (0 when the decision carries none).
func TestClassifyReferenceRows(t *testing.T) {
	cases := []struct {
		name       string
		sql        string
		route      planner.Route
		reason     planner.Reason
		saferSteps int
	}{
		// Column operations.
		{"add column plain", "ALTER TABLE t ADD COLUMN age int", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"add column constant default", "ALTER TABLE t ADD COLUMN age int DEFAULT 0", planner.RouteNative, planner.ReasonFastDefault, 0},
		{"add column volatile default now", "ALTER TABLE t ADD COLUMN created timestamptz DEFAULT now()", planner.RouteCopyAndSwap, planner.ReasonVolatileDefault, 0},
		{"add column volatile default random", "ALTER TABLE t ADD COLUMN r float8 DEFAULT random()", planner.RouteCopyAndSwap, planner.ReasonVolatileDefault, 0},
		{"add column volatile default uuid", "ALTER TABLE t ADD COLUMN id uuid DEFAULT uuid_generate_v4()", planner.RouteCopyAndSwap, planner.ReasonVolatileDefault, 0},
		{"add column generated stored", "ALTER TABLE t ADD COLUMN total numeric GENERATED ALWAYS AS (price * qty) STORED", planner.RouteCopyAndSwap, planner.ReasonGeneratedStored, 0},
		{"drop column", "ALTER TABLE t DROP COLUMN age", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"alter type widen varchar", "ALTER TABLE t ALTER COLUMN v50 TYPE varchar(100)", planner.RouteNative, planner.ReasonBinaryCoercible, 0},
		{"alter type varchar to text", "ALTER TABLE t ALTER COLUMN v50 TYPE text", planner.RouteNative, planner.ReasonBinaryCoercible, 0},
		{"alter type drop varchar bound", "ALTER TABLE t ALTER COLUMN v50 TYPE varchar", planner.RouteNative, planner.ReasonBinaryCoercible, 0},
		{"alter type widen numeric precision", "ALTER TABLE t ALTER COLUMN num TYPE numeric(12,2)", planner.RouteNative, planner.ReasonBinaryCoercible, 0},
		{"alter type shrink varchar", "ALTER TABLE t ALTER COLUMN v50 TYPE varchar(10)", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, 0},
		{"alter type bound an unbounded varchar", "ALTER TABLE t ALTER COLUMN vany TYPE varchar(50)", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, 0},
		{"alter type numeric scale change", "ALTER TABLE t ALTER COLUMN num TYPE numeric(12,4)", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, 0},
		{"alter type general int to bigint", "ALTER TABLE t ALTER COLUMN i TYPE bigint", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, 0},
		{"alter type text to jsonb with using", "ALTER TABLE t ALTER COLUMN txt TYPE jsonb USING txt::jsonb", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, 0},
		{"alter type unknown column", "ALTER TABLE t ALTER COLUMN mystery TYPE text", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, 0},
		{"set default", "ALTER TABLE t ALTER COLUMN age SET DEFAULT 1", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"drop default", "ALTER TABLE t ALTER COLUMN age DROP DEFAULT", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"set not null", "ALTER TABLE t ALTER COLUMN age SET NOT NULL", planner.RouteNative, planner.ReasonSaferIdiom, 4},
		{"drop not null", "ALTER TABLE t ALTER COLUMN age DROP NOT NULL", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"rename column", "ALTER TABLE t RENAME COLUMN a TO b", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"set statistics", "ALTER TABLE t ALTER COLUMN age SET STATISTICS 500", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"set storage", "ALTER TABLE t ALTER COLUMN blob SET STORAGE EXTERNAL", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"set column options", "ALTER TABLE t ALTER COLUMN age SET (n_distinct = 100)", planner.RouteNative, planner.ReasonMetadataOnly, 0},

		// Index operations.
		{"create index", "CREATE INDEX i ON t (a)", planner.RouteNative, planner.ReasonSaferIdiom, 1},
		{"create index concurrently", "CREATE INDEX CONCURRENTLY i ON t (a)", planner.RouteNative, planner.ReasonOnlineIdiom, 0},
		{"drop index", "DROP INDEX i", planner.RouteNative, planner.ReasonSaferIdiom, 1},
		{"drop index concurrently", "DROP INDEX CONCURRENTLY i", planner.RouteNative, planner.ReasonOnlineIdiom, 0},
		{"reindex", "REINDEX INDEX i", planner.RouteNative, planner.ReasonSaferIdiom, 1},
		{"reindex concurrently", "REINDEX INDEX CONCURRENTLY i", planner.RouteNative, planner.ReasonOnlineIdiom, 0},
		{"rename index", "ALTER INDEX i RENAME TO i2", planner.RouteNative, planner.ReasonMetadataOnly, 0},

		// Constraint operations.
		{"add primary key direct", "ALTER TABLE t ADD CONSTRAINT t_pkey PRIMARY KEY (id)", planner.RouteNative, planner.ReasonSaferIdiom, 2},
		{"add unique direct", "ALTER TABLE t ADD CONSTRAINT u UNIQUE (a, b)", planner.RouteNative, planner.ReasonSaferIdiom, 2},
		{"add primary key using index", "ALTER TABLE t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey", planner.RouteNative, planner.ReasonOnlineIdiom, 0},
		{"add check", "ALTER TABLE t ADD CONSTRAINT c CHECK (age > 0)", planner.RouteNative, planner.ReasonSaferIdiom, 2},
		{"add foreign key", "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (pid) REFERENCES p (id)", planner.RouteNative, planner.ReasonSaferIdiom, 2},
		{"add check not valid", "ALTER TABLE t ADD CONSTRAINT c CHECK (age > 0) NOT VALID", planner.RouteNative, planner.ReasonOnlineIdiom, 0},
		{"add foreign key not valid", "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (pid) REFERENCES p (id) NOT VALID", planner.RouteNative, planner.ReasonOnlineIdiom, 0},
		{"add unnamed check", "ALTER TABLE t ADD CHECK (age > 0)", planner.RouteNative, planner.ReasonSaferIdiom, 0},
		{"add exclusion", "ALTER TABLE t ADD CONSTRAINT ex EXCLUDE USING gist (room WITH =)", planner.RouteRefuse, planner.ReasonUnsupportedOperation, 0},
		{"validate constraint", "ALTER TABLE t VALIDATE CONSTRAINT c", planner.RouteNative, planner.ReasonOnlineIdiom, 0},
		{"drop constraint", "ALTER TABLE t DROP CONSTRAINT c", planner.RouteNative, planner.ReasonMetadataOnly, 0},

		// Table and partition operations.
		{"rename table", "ALTER TABLE t RENAME TO t2", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"set schema", "ALTER TABLE t SET SCHEMA s2", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"set tablespace", "ALTER TABLE t SET TABLESPACE fast", planner.RouteCopyAndSwap, planner.ReasonRelocation, 0},
		{"set fillfactor", "ALTER TABLE t SET (fillfactor = 70)", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"cluster", "CLUSTER t USING i", planner.RouteRefuse, planner.ReasonUnsupportedOperation, 0},
		{"vacuum full", "VACUUM FULL t", planner.RouteRefuse, planner.ReasonUnsupportedOperation, 0},
		{"attach partition", "ALTER TABLE t ATTACH PARTITION p FOR VALUES FROM (1) TO (10)", planner.RouteNative, planner.ReasonSaferIdiom, 0},
		{"detach partition", "ALTER TABLE t DETACH PARTITION p", planner.RouteNative, planner.ReasonSaferIdiom, 1},
		{"detach partition concurrently", "ALTER TABLE t DETACH PARTITION p CONCURRENTLY", planner.RouteNative, planner.ReasonOnlineIdiom, 0},

		// Non-DDL and unknown statements.
		{"dml", "INSERT INTO t VALUES (1)", planner.RouteRefuse, planner.ReasonUnsupportedOperation, 0},
		{"create table", "CREATE TABLE t (id int PRIMARY KEY)", planner.RouteNative, planner.ReasonMetadataOnly, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := classifyOne(t, tc.sql)
			assert.Equal(t, tc.route, d.Route)
			assert.Equal(t, tc.reason, d.Reason)
			assert.Len(t, d.SaferSQL, tc.saferSteps)
		})
	}
}

func TestClassifySetNotNullSequence(t *testing.T) {
	d := classifyOne(t, "ALTER TABLE s.t ALTER COLUMN age SET NOT NULL")
	assert.Equal(t, []string{
		`ALTER TABLE "s"."t" ADD CONSTRAINT "t_age_not_null" CHECK ("age" IS NOT NULL) NOT VALID`,
		`ALTER TABLE "s"."t" VALIDATE CONSTRAINT "t_age_not_null"`,
		`ALTER TABLE "s"."t" ALTER COLUMN "age" SET NOT NULL`,
		`ALTER TABLE "s"."t" DROP CONSTRAINT "t_age_not_null"`,
	}, d.SaferSQL)
}

func TestClassifyAddPrimaryKeySequence(t *testing.T) {
	d := classifyOne(t, "ALTER TABLE s.t ADD CONSTRAINT t_pkey PRIMARY KEY (id)")
	assert.Equal(t, []string{
		`CREATE UNIQUE INDEX CONCURRENTLY "t_pkey" ON "s"."t" ("id")`,
		`ALTER TABLE "s"."t" ADD CONSTRAINT "t_pkey" PRIMARY KEY USING INDEX "t_pkey"`,
	}, d.SaferSQL)
}

func TestClassifyAddCheckSequenceValidates(t *testing.T) {
	d := classifyOne(t, "ALTER TABLE t ADD CONSTRAINT c CHECK (age > 0)")
	require.Len(t, d.SaferSQL, 2)
	// The rewritten first step must parse back as NOT VALID; the second
	// step is the online validation of the same constraint.
	assert.Contains(t, d.SaferSQL[1], "VALIDATE CONSTRAINT")
}

func TestClassifyAggregatesWorstRoute(t *testing.T) {
	plan, err := planner.Classify(
		"ALTER TABLE t ADD COLUMN a int, ADD COLUMN created timestamptz DEFAULT now()", facts)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 2)
	assert.Equal(t, planner.RouteNative, plan.Decisions[0].Route)
	assert.Equal(t, planner.RouteCopyAndSwap, plan.Decisions[1].Route)
	assert.Equal(t, planner.RouteCopyAndSwap, plan.Route, "one rewrite makes the statement a copy")
}

func TestClassifyRefusalDominatesAggregate(t *testing.T) {
	plan, err := planner.Classify(
		"ALTER TABLE t ADD COLUMN created timestamptz DEFAULT now(), ENABLE ROW LEVEL SECURITY", facts)
	require.NoError(t, err)
	assert.Equal(t, planner.RouteRefuse, plan.Route, "a refused operation refuses the statement")
}

func TestClassifyMultiOpStatementCarriesNoRewrites(t *testing.T) {
	plan, err := planner.Classify(
		"ALTER TABLE t ALTER COLUMN age SET NOT NULL, DROP COLUMN b", facts)
	require.NoError(t, err)
	for _, d := range plan.Decisions {
		assert.Empty(t, d.SaferSQL, "multi-operation statements must not carry partial rewrites")
	}
}

func TestClassifyNoFactsIsConservative(t *testing.T) {
	plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN v50 TYPE varchar(100)", planner.Facts{})
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, plan.Decisions[0].Route,
		"without live column facts every type change is a rewrite")
}

func TestClassifyParseErrorSurfaces(t *testing.T) {
	_, err := planner.Classify("ALTER TABLE", planner.Facts{})
	assert.Error(t, err)
}
