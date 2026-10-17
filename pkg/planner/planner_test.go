package planner_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// FactsFrom is the one seam between live introspection and classification:
// every column's canonical type must come through so type-narrowing
// decisions see the real live types.
func TestFactsFromExtractsColumnTypes(t *testing.T) {
	live := schemadiff.Model{Columns: []schemadiff.Column{
		{Name: "id", Type: "bigint"},
		{Name: "name", Type: "character varying(50)"},
	}}
	assert.Equal(t, planner.Facts{ColumnTypes: map[string]string{
		"id":   "bigint",
		"name": "character varying(50)",
	}}, planner.FactsFrom(live))
}

// FactsFrom carries what the live table's replica identity keys on, so a
// drop of it is judged from the same facts on every front door.
func TestFactsFromExtractsReplicaIdentity(t *testing.T) {
	keys := schemadiff.ReplicaIdentityKeys{
		Indexes:     []string{"t_ref_key"},
		Constraints: []string{"t_ref_key"},
		Columns:     []string{"ref", "tenant"},
	}
	live := schemadiff.Model{
		Columns:         []schemadiff.Column{{Name: "id", Type: "bigint"}},
		ReplicaIdentity: keys,
	}
	assert.Equal(t, keys, planner.FactsFrom(live).ReplicaIdentity)
}

// Dropping what REPLICA IDENTITY USING INDEX keys on leaves the table with
// no usable identity, so UPDATE and DELETE fail under a publication. The
// planner refuses the index drop in every form, the drop of the constraint
// that owns the index, and the drop of any key column, which takes the
// index with it. Everything else on the table classifies as before, and a
// table whose identity is not an index has nothing to protect.
func TestClassifyRefusesDroppingTheReplicaIdentity(t *testing.T) {
	identity := planner.Facts{
		ColumnTypes: map[string]string{"tenant": "bigint", "ref": "text", "note": "text"},
		ReplicaIdentity: schemadiff.ReplicaIdentityKeys{
			Indexes:     []string{"t_ref_key"},
			Constraints: []string{"t_ref_key"},
			Columns:     []string{"ref", "tenant"},
		},
	}
	refused := []string{
		"DROP INDEX t_ref_key",
		"DROP INDEX CONCURRENTLY t_ref_key",
		"DROP INDEX IF EXISTS public.t_ref_key",
		"DROP INDEX t_other_idx, t_ref_key",
		"ALTER TABLE t DROP CONSTRAINT t_ref_key",
		"ALTER TABLE t DROP COLUMN ref",
		"ALTER TABLE t DROP COLUMN tenant CASCADE",
	}
	for _, sql := range refused {
		plan, err := planner.Classify(sql, identity)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		d := plan.Decisions[0]
		assert.Equal(t, planner.RouteRefuse, d.Route, sql)
		assert.Equal(t, planner.ReasonReplicaIdentity, d.Reason, sql)
		assert.True(t, d.Destructive, sql)
		assert.Equal(t, planner.RouteRefuse, plan.Route, sql)
	}

	allowed := map[string]planner.Reason{
		"DROP INDEX CONCURRENTLY t_other_idx":        planner.ReasonOnlineIdiom,
		"ALTER TABLE t DROP CONSTRAINT t_note_check": planner.ReasonMetadataOnly,
		"ALTER TABLE t DROP COLUMN note":             planner.ReasonMetadataOnly,
		"ALTER TABLE t ADD COLUMN extra int":         planner.ReasonMetadataOnly,
	}
	for sql, reason := range allowed {
		plan, err := planner.Classify(sql, identity)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		assert.Equal(t, reason, plan.Decisions[0].Reason, sql)
	}

	noIndexIdentity := identity
	noIndexIdentity.ReplicaIdentity = schemadiff.ReplicaIdentityKeys{}
	plan, err := planner.Classify("ALTER TABLE t DROP COLUMN ref", noIndexIdentity)
	require.NoError(t, err)
	assert.Equal(t, planner.ReasonMetadataOnly, plan.Decisions[0].Reason)
}

// A column dropped under ONLY stays in the tables below, so only the
// table's own identity index counts: a column only a child's identity uses
// classifies as an ordinary drop, while one the table's own identity uses
// is refused either way.
func TestClassifyJudgesAnOnlyDropByTheTablesOwnIdentity(t *testing.T) {
	facts := planner.Facts{
		ColumnTypes: map[string]string{"ref": "text", "code": "text"},
		ReplicaIdentity: schemadiff.ReplicaIdentityKeys{
			Columns:    []string{"code", "ref"},
			OwnColumns: []string{"code"},
		},
	}
	for sql, reason := range map[string]planner.Reason{
		"ALTER TABLE ONLY parent DROP COLUMN ref":  planner.ReasonMetadataOnly,
		"ALTER TABLE parent DROP COLUMN ref":       planner.ReasonReplicaIdentity,
		"ALTER TABLE ONLY parent DROP COLUMN code": planner.ReasonReplicaIdentity,
	} {
		plan, err := planner.Classify(sql, facts)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		assert.Equal(t, reason, plan.Decisions[0].Reason, sql)
	}
}

// facts mirrors a live table whose column types exercise both sides of the
// binary-coercible rules.
var facts = planner.Facts{ColumnTypes: map[string]string{
	"v50":  "character varying(50)",
	"vany": "character varying",
	"num":  "numeric(10,2)",
	"i":    "integer",
	"txt":  "text",
	"varr": "character varying(50)[]",
	"narr": "numeric(10,2)[]",
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
// drops of columns, constraints, indexes, and NOT NULL discard live
// structure or a guarantee, and every front door that routes through the
// classifier inherits the same marking — including DROP INDEX, whose drop
// discards the index's guarantee (uniqueness, for a unique index) however
// it is submitted. DROP DEFAULT stays non-destructive: a default
// guarantees nothing about existing rows and is recreated by a
// metadata-only statement.
func TestClassifyMarksDropsDestructive(t *testing.T) {
	destructive := []string{
		"ALTER TABLE t DROP COLUMN age",
		"ALTER TABLE t DROP CONSTRAINT t_age_check",
		"DROP INDEX t_v_idx",
		"DROP INDEX CONCURRENTLY t_v_idx",
		"ALTER TABLE t ALTER COLUMN age DROP NOT NULL",
	}
	for _, sql := range destructive {
		assert.True(t, classifyOne(t, sql).Destructive, sql)
	}
	nonDestructive := []string{
		"ALTER TABLE t ADD COLUMN age int",
		"ALTER TABLE t ALTER COLUMN v50 TYPE varchar(100)",
		"ALTER TABLE t ALTER COLUMN age DROP DEFAULT",
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

// The scalar relabel rules do not carry over to arrays: PostgreSQL converts
// an array column element by element and rewrites the table for any element
// change, widenings included. Only an unchanged element type is a relabel,
// whatever dimensions are declared. A target qualified with a schema other
// than pg_catalog is a user type, never the built-in that shares its name.
// TestBinaryCoercibleVerdictMatchesTheServer proves each row on a server.
func TestClassifyArrayAndQualifiedTypeChanges(t *testing.T) {
	cases := []struct {
		sql   string
		route planner.Route
	}{
		{"ALTER TABLE t ALTER COLUMN varr TYPE text", planner.RouteCopyAndSwap},
		{"ALTER TABLE t ALTER COLUMN varr TYPE varchar(100)", planner.RouteCopyAndSwap},
		{"ALTER TABLE t ALTER COLUMN varr TYPE varchar(100)[]", planner.RouteCopyAndSwap},
		{"ALTER TABLE t ALTER COLUMN varr TYPE varchar[]", planner.RouteCopyAndSwap},
		{"ALTER TABLE t ALTER COLUMN varr TYPE text[]", planner.RouteCopyAndSwap},
		{"ALTER TABLE t ALTER COLUMN narr TYPE numeric(12,2)[]", planner.RouteCopyAndSwap},
		{"ALTER TABLE t ALTER COLUMN v50 TYPE text[]", planner.RouteCopyAndSwap},
		{"ALTER TABLE t ALTER COLUMN v50 TYPE app.text", planner.RouteCopyAndSwap},
		{"ALTER TABLE t ALTER COLUMN varr TYPE varchar(50)[]", planner.RouteNative},
		{"ALTER TABLE t ALTER COLUMN varr TYPE varchar(50)[][]", planner.RouteNative},
		{"ALTER TABLE t ALTER COLUMN varr TYPE varchar(50) ARRAY", planner.RouteNative},
		{"ALTER TABLE t ALTER COLUMN v50 TYPE pg_catalog.text", planner.RouteNative},
	}
	for _, tc := range cases {
		d := classifyOne(t, tc.sql)
		assert.Equal(t, tc.route, d.Route, tc.sql)
		if tc.route == planner.RouteNative {
			assert.Equal(t, planner.ReasonBinaryCoercible, d.Reason, tc.sql)
		} else {
			assert.Equal(t, planner.ReasonTypeRewrite, d.Reason, tc.sql)
			assert.False(t, d.Unverified, "a proven rewrite is not unverified: %s", tc.sql)
		}
	}
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
			if tc.saferSteps > 0 {
				assert.Equal(t, planner.ExecutionAutocommit, d.SaferSQLExecution,
					"a constructed sequence carries its execution contract")
			} else {
				assert.Empty(t, d.SaferSQLExecution,
					"the contract is present exactly when SaferSQL is")
			}
		})
	}
}

func TestClassifySetNotNullSequence(t *testing.T) {
	d := classifyOne(t, "ALTER TABLE s.t ALTER COLUMN age SET NOT NULL")
	assert.Equal(t, []string{
		`ALTER TABLE "s"."t" ADD CONSTRAINT "t_age_not_null_check" CHECK ("age" IS NOT NULL) NOT VALID`,
		`ALTER TABLE "s"."t" VALIDATE CONSTRAINT "t_age_not_null_check"`,
		`ALTER TABLE "s"."t" ALTER COLUMN "age" SET NOT NULL`,
		`ALTER TABLE "s"."t" DROP CONSTRAINT "t_age_not_null_check"`,
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

// An added column classifies by what the server proved about the rewrite
// before the statement's shape: a constant default on a constrained domain
// still rewrites, and a default the catalog stores runs in place however it
// is written. A proven rewrite takes its reason from what the server found
// rewrites, the type or the default, never from how the default is
// written. A column the server could not prove, or that no proof was run
// for, keeps the shape rules, and an expression default there fails closed
// as unverified.
func TestClassifyAddColumnByTheProvenRewrite(t *testing.T) {
	proven := planner.Facts{AddColumnProofs: map[string]statement.AddColumnProof{
		"code":    statement.AddColumnTypeRewrites,
		"strict":  statement.AddColumnTypeRewrites,
		"scored":  statement.AddColumnTypeRewrites,
		"id":      statement.AddColumnDefaultRewrites,
		"created": statement.AddColumnInPlace,
		"checked": statement.AddColumnInPlace,
		"age":     statement.AddColumnInPlace,
		"high":    statement.AddColumnUnproven,
		"capped":  statement.AddColumnUnproven,
	}}
	cases := []struct {
		name       string
		sql        string
		route      planner.Route
		reason     planner.Reason
		unverified bool
	}{
		{"constrained domain with a constant default", "ALTER TABLE t ADD COLUMN code positive_int DEFAULT 1", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, false},
		{"constrained domain without a default", "ALTER TABLE t ADD COLUMN strict positive_int", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, false},
		{"constrained domain with a stable default", "ALTER TABLE t ADD COLUMN scored positive_int DEFAULT next_score()", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, false},
		{"volatile default", "ALTER TABLE t ADD COLUMN id uuid DEFAULT gen_random_uuid()", planner.RouteCopyAndSwap, planner.ReasonVolatileDefault, false},
		{"stable default stored in the catalog", "ALTER TABLE t ADD COLUMN created timestamptz NOT NULL DEFAULT now()", planner.RouteNative, planner.ReasonFastDefault, false},
		{"stored default with an inline check", "ALTER TABLE t ADD COLUMN checked timestamptz DEFAULT now() CHECK (checked > '2000-01-01')", planner.RouteNative, planner.ReasonSaferIdiom, false},
		{"plain column", "ALTER TABLE t ADD COLUMN age int", planner.RouteNative, planner.ReasonMetadataOnly, false},
		{"expression default the server could not prove", "ALTER TABLE t ADD COLUMN high bigint DEFAULT max_id()", planner.RouteCopyAndSwap, planner.ReasonVolatileDefault, true},
		{"constant default the server could not prove", "ALTER TABLE t ADD COLUMN capped bounded DEFAULT 5", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite, true},
		{"expression default with no proof run", "ALTER TABLE t ADD COLUMN other timestamptz DEFAULT now()", planner.RouteCopyAndSwap, planner.ReasonVolatileDefault, true},
		{"constant default with no proof run", "ALTER TABLE t ADD COLUMN other int DEFAULT 0", planner.RouteNative, planner.ReasonFastDefault, false},
		{"generated stored whatever the proof says", "ALTER TABLE t ADD COLUMN age int GENERATED ALWAYS AS (1) STORED", planner.RouteCopyAndSwap, planner.ReasonGeneratedStored, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planner.Classify(tc.sql, proven)
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			assert.Equal(t, tc.route, plan.Decisions[0].Route)
			assert.Equal(t, tc.reason, plan.Decisions[0].Reason)
			assert.Equal(t, tc.unverified, plan.Decisions[0].Unverified)
		})
	}
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
		planner.ReasonAppBreakingRename,
		planner.ReasonVolatileDefault,
		planner.ReasonGeneratedStored,
		planner.ReasonTypeRewrite,
		planner.ReasonRelocation,
		planner.ReasonPartitionParentLock,
		planner.ReasonReplicaIdentity,
		planner.ReasonUnsupportedOperation,
	}, planner.Reasons())
}
