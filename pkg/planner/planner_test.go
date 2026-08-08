package planner_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
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

// Destructive is a decision-level fact derived from the operation shape:
// drops of columns, constraints, and indexes discard live structure, and
// every front door that routes through the classifier inherits the same
// marking — including DROP INDEX, whose drop discards the index's
// guarantee (uniqueness, for a unique index) however it is submitted.
func TestClassifyMarksDropsDestructive(t *testing.T) {
	destructive := []string{
		"ALTER TABLE t DROP COLUMN age",
		"ALTER TABLE t DROP CONSTRAINT t_age_check",
		"DROP INDEX t_v_idx",
		"DROP INDEX CONCURRENTLY t_v_idx",
	}
	for _, sql := range destructive {
		assert.True(t, classifyOne(t, sql).Destructive, sql)
	}
	nonDestructive := []string{
		"ALTER TABLE t ADD COLUMN age int",
		"ALTER TABLE t ALTER COLUMN v50 TYPE varchar(100)",
		"ALTER TABLE t ALTER COLUMN age DROP DEFAULT",
		"ALTER TABLE t ALTER COLUMN age DROP NOT NULL",
		"CREATE INDEX t_v_idx ON t (v50)",
	}
	for _, sql := range nonDestructive {
		assert.False(t, classifyOne(t, sql).Destructive, sql)
	}
}

// Unverified separates "the planner proved a rewrite" from "the planner
// failed closed for lack of facts": the same conversion is a proven
// relabel with a column fact, and an unverified rewrite without one. A
// USING clause is a rewrite regardless of facts, so it is never
// unverified.
func TestClassifyUnverifiedMarksFactlessTypeChanges(t *testing.T) {
	verified, err := planner.Classify(
		"ALTER TABLE t ALTER COLUMN v50 TYPE varchar(100)", facts)
	require.NoError(t, err)
	require.Len(t, verified.Decisions, 1)
	assert.Equal(t, planner.RouteNative, verified.Decisions[0].Route)
	assert.False(t, verified.Decisions[0].Unverified)

	factless, err := planner.Classify(
		"ALTER TABLE t ALTER COLUMN v50 TYPE varchar(100)", planner.Facts{})
	require.NoError(t, err)
	require.Len(t, factless.Decisions, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, factless.Decisions[0].Route)
	assert.True(t, factless.Decisions[0].Unverified)

	using, err := planner.Classify(
		"ALTER TABLE t ALTER COLUMN txt TYPE jsonb USING txt::jsonb", planner.Facts{})
	require.NoError(t, err)
	require.Len(t, using.Decisions, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, using.Decisions[0].Route)
	assert.False(t, using.Decisions[0].Unverified)
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
		{"add column inline unique", "ALTER TABLE t ADD COLUMN c int UNIQUE", planner.RouteNative, planner.ReasonSaferIdiom, 0},
		{"add column inline primary key", "ALTER TABLE t ADD COLUMN c int PRIMARY KEY", planner.RouteNative, planner.ReasonSaferIdiom, 0},
		{"add column inline foreign key", "ALTER TABLE t ADD COLUMN c int REFERENCES p (id)", planner.RouteNative, planner.ReasonSaferIdiom, 0},
		{"add column inline check", "ALTER TABLE t ADD COLUMN c int CHECK (c > 0)", planner.RouteNative, planner.ReasonSaferIdiom, 0},
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
		{"rename column", "ALTER TABLE t RENAME COLUMN a TO b", planner.RouteNative, planner.ReasonAppBreakingRename, 0},
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
		{"rename table", "ALTER TABLE t RENAME TO t2", planner.RouteNative, planner.ReasonAppBreakingRename, 0},
		{"set schema", "ALTER TABLE t SET SCHEMA s2", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"set tablespace", "ALTER TABLE t SET TABLESPACE fast", planner.RouteCopyAndSwap, planner.ReasonRelocation, 0},
		{"set fillfactor", "ALTER TABLE t SET (fillfactor = 70)", planner.RouteNative, planner.ReasonMetadataOnly, 0},
		{"cluster", "CLUSTER t USING i", planner.RouteRefuse, planner.ReasonUnsupportedOperation, 0},
		{"vacuum full", "VACUUM FULL t", planner.RouteRefuse, planner.ReasonUnsupportedOperation, 0},
		{"attach partition", "ALTER TABLE t ATTACH PARTITION p FOR VALUES FROM (1) TO (10)", planner.RouteNative, planner.ReasonSaferIdiom, 0},
		{"detach partition", "ALTER TABLE t DETACH PARTITION p", planner.RouteNative, planner.ReasonSaferIdiom, 1},
		{"detach partition concurrently", "ALTER TABLE t DETACH PARTITION p CONCURRENTLY", planner.RouteNative, planner.ReasonOnlineIdiom, 0},
		{"create table partition of", "CREATE TABLE p PARTITION OF t FOR VALUES FROM (1) TO (10)", planner.RouteNative, planner.ReasonPartitionParentLock, 0},

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

// generatedConstraintName parses a safer-sequence ADD CONSTRAINT step back
// through the statement parser and returns the typed constraint name, so
// the test asserts identifier facts rather than SQL prose.
func generatedConstraintName(t *testing.T, step string) string {
	t.Helper()
	ops, err := statement.ParseOps(step)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	require.Equal(t, statement.OpAddConstraint, ops[0].Kind)
	require.NotEmpty(t, ops[0].Name)
	return ops[0].Name
}

func TestClassifyGeneratedNamesFitIdentifierLimit(t *testing.T) {
	// Long enough that table + column + suffix would exceed PostgreSQL's
	// 63-byte identifier limit, where the server would silently truncate.
	table := strings.Repeat("t", 40)
	colA := strings.Repeat("a", 40)
	colB := strings.Repeat("b", 40)

	dA := classifyOne(t, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, colA))
	dB := classifyOne(t, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, colB))
	require.NotEmpty(t, dA.SaferSQL)
	require.NotEmpty(t, dB.SaferSQL)

	nameA := generatedConstraintName(t, dA.SaferSQL[0])
	nameB := generatedConstraintName(t, dB.SaferSQL[0])
	assert.LessOrEqual(t, len(nameA), 63, "generated names must fit PostgreSQL's identifier limit")
	assert.LessOrEqual(t, len(nameB), 63)
	assert.NotEqual(t, nameA, nameB,
		"columns differing only past the truncation point must not collide")

	// Deterministic: the same input yields the same fitted name.
	dA2 := classifyOne(t, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, colA))
	assert.Equal(t, dA.SaferSQL, dA2.SaferSQL)
}

// The route and reason vocabularies are closed sets a consumer branches on;
// both are pinned to plan-report format_version 1 (docs/plan-report.md). A
// new value here without a format_version bump is a contract break.
func TestRoutesVocabularyPinned(t *testing.T) {
	assert.Equal(t, []planner.Route{
		planner.RouteNative,
		planner.RouteCopyAndSwap,
		planner.RouteRefuse,
	}, planner.Routes())
}

func TestReasonsVocabularyPinned(t *testing.T) {
	assert.Equal(t, []planner.Reason{
		planner.ReasonMetadataOnly,
		planner.ReasonOnlineIdiom,
		planner.ReasonFastDefault,
		planner.ReasonBinaryCoercible,
		planner.ReasonSaferIdiom,
		planner.ReasonVolatileDefault,
		planner.ReasonGeneratedStored,
		planner.ReasonTypeRewrite,
		planner.ReasonRelocation,
		planner.ReasonPartitionParentLock,
		planner.ReasonUnsupportedOperation,
	}, planner.Reasons())
}
