package planner_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// FactsFrom is the one seam between live introspection and classification:
// every column's canonical type must come through so type-narrowing
// decisions see the real live types, and a column's own collation must
// come through so retyping it is never read as a relabel.
func TestFactsFromExtractsColumnTypes(t *testing.T) {
	live := schemadiff.Model{Columns: []schemadiff.Column{
		{Name: "id", Type: "bigint"},
		{Name: "name", Type: "character varying(50)"},
		{Name: "code", Type: "text", Collation: `pg_catalog."C"`},
	}}
	assert.Equal(t, planner.Facts{
		ColumnTypes: map[string]string{
			"id":   "bigint",
			"name": "character varying(50)",
			"code": "text",
		},
		ColumnCollations: map[string]string{"code": `pg_catalog."C"`},
		Storage:          planner.StorageHeap,
	}, planner.FactsFrom(live))
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

// FactsFrom marks a table with a partition key as a partitioned parent, so
// an operation the server runs differently on one, such as SET TABLESPACE,
// is judged from the same fact on every front door. A partition has no key
// of its own and, like an ordinary table, has a heap.
func TestFactsFromExtractsStorage(t *testing.T) {
	parent := schemadiff.Model{
		Columns:      []schemadiff.Column{{Name: "created_at", Type: "timestamp with time zone"}},
		PartitionKey: "RANGE (created_at)",
	}
	partition := schemadiff.Model{
		Columns:     []schemadiff.Column{{Name: "created_at", Type: "timestamp with time zone"}},
		IsPartition: true,
	}
	ordinary := schemadiff.Model{
		Columns: []schemadiff.Column{{Name: "created_at", Type: "timestamp with time zone"}},
	}
	assert.Equal(t, planner.StoragePartitionedParent, planner.FactsFrom(parent).Storage)
	assert.Equal(t, planner.StorageHeap, planner.FactsFrom(partition).Storage)
	assert.Equal(t, planner.StorageHeap, planner.FactsFrom(ordinary).Storage)
}

// SET TABLESPACE moves an ordinary table's heap, a copy under ACCESS
// EXCLUSIVE for as long as it takes, so it routes to copy-and-swap. A
// partitioned parent has no heap: the server records the tablespace as
// the default for partitions created later, under a brief ACCESS EXCLUSIVE
// on the parent alone, and moves no partition, so it is metadata-only.
func TestClassifySetTablespaceOnAPartitionedParentIsMetadataOnly(t *testing.T) {
	const sql = "ALTER TABLE app.events SET TABLESPACE archive"

	parent, err := planner.Classify(sql, planner.Facts{Storage: planner.StoragePartitionedParent})
	require.NoError(t, err)
	require.Len(t, parent.Decisions, 1)
	assert.Equal(t, planner.RouteNative, parent.Decisions[0].Route)
	assert.Equal(t, planner.ReasonMetadataOnly, parent.Decisions[0].Reason)
	assert.False(t, parent.Decisions[0].Unverified)
	assert.Empty(t, parent.Decisions[0].SaferSQL, "the statement runs as written")

	heap, err := planner.Classify(sql, planner.Facts{Storage: planner.StorageHeap})
	require.NoError(t, err)
	require.Len(t, heap.Decisions, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, heap.Decisions[0].Route)
	assert.Equal(t, planner.ReasonRelocation, heap.Decisions[0].Reason)
	assert.False(t, heap.Decisions[0].Unverified, "a heap read live is a proven move")
}

// Without the storage fact, as in an offline lint, the planner cannot tell
// a heap from a partitioned parent: it fails closed to the move and marks
// the decision unverified, since the same statement may be metadata-only.
func TestClassifySetTablespaceWithoutStorageIsAnUnverifiedRelocation(t *testing.T) {
	plan, err := planner.Classify("ALTER TABLE app.events SET TABLESPACE archive", planner.Facts{})
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.RouteCopyAndSwap, plan.Decisions[0].Route)
	assert.Equal(t, planner.ReasonRelocation, plan.Decisions[0].Reason)
	assert.True(t, plan.Decisions[0].Unverified)
}

// ALTER TABLE ONLY ... SET TABLESPACE on a partitioned parent is the same
// catalog change as the plain form: the subcommand never recurses.
func TestClassifySetTablespaceOnlyOnAPartitionedParentIsMetadataOnly(t *testing.T) {
	plan, err := planner.Classify("ALTER TABLE ONLY app.events SET TABLESPACE archive",
		planner.Facts{Storage: planner.StoragePartitionedParent})
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.RouteNative, plan.Decisions[0].Route)
	assert.Equal(t, planner.ReasonMetadataOnly, plan.Decisions[0].Reason)
}

// ALTER TABLE ALL IN TABLESPACE moves every table in a tablespace, heaps
// included, so the partitioned-parent fact of one table cannot make it
// metadata-only: it is refused.
func TestClassifyAlterTableAllInTablespaceIsRefused(t *testing.T) {
	plan, err := planner.Classify("ALTER TABLE ALL IN TABLESPACE pg_default SET TABLESPACE archive",
		planner.Facts{Storage: planner.StoragePartitionedParent})
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.RouteRefuse, plan.Decisions[0].Route)
	assert.Equal(t, planner.ReasonUnsupportedOperation, plan.Decisions[0].Reason)
}

// ALTER INDEX ... SET TABLESPACE rebuilds the index's storage in the new
// tablespace; the imperative front door does not take it.
func TestClassifyAlterIndexSetTablespaceIsRefused(t *testing.T) {
	plan, err := planner.Classify("ALTER INDEX app.events_pkey SET TABLESPACE archive",
		planner.Facts{Storage: planner.StoragePartitionedParent})
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.RouteRefuse, plan.Decisions[0].Route)
	assert.Equal(t, planner.ReasonUnsupportedOperation, plan.Decisions[0].Reason)
}

// ALTER MATERIALIZED VIEW ... SET TABLESPACE moves the view's heap; the
// imperative front door does not take it.
func TestClassifyAlterMaterializedViewSetTablespaceIsRefused(t *testing.T) {
	plan, err := planner.Classify("ALTER MATERIALIZED VIEW app.daily_totals SET TABLESPACE archive",
		planner.Facts{Storage: planner.StoragePartitionedParent})
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.RouteRefuse, plan.Decisions[0].Route)
	assert.Equal(t, planner.ReasonUnsupportedOperation, plan.Decisions[0].Reason)
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

// PostgreSQL refuses to retype or drop a column a view reads, so either
// change on such a column is refused with the views named, even a type
// change that would otherwise relabel in place. A rename, which the view
// follows, and changes to columns no view reads classify as usual.
func TestClassifyRefusesChangingAColumnAViewReads(t *testing.T) {
	views := planner.Facts{
		ColumnTypes: map[string]string{"email": "character varying(50)", "note": "text"},
		DependentViews: map[string][]string{
			"email": {`"app"."active_users"`, `"reports"."emails"`},
		},
		DropDependentViews: map[string][]string{
			"email": {`"app"."active_users"`, `"reports"."emails"`},
		},
		OwnDependentViews: map[string][]string{
			"email": {`"app"."active_users"`, `"reports"."emails"`},
		},
	}
	for _, sql := range []string{
		"ALTER TABLE t ALTER COLUMN email TYPE varchar(100)",
		"ALTER TABLE t ALTER COLUMN email TYPE text",
		"ALTER TABLE t ALTER COLUMN email TYPE bigint USING email::bigint",
		"ALTER TABLE t DROP COLUMN email",
		"ALTER TABLE ONLY t DROP COLUMN email",
	} {
		plan, err := planner.Classify(sql, views)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		d := plan.Decisions[0]
		assert.Equal(t, planner.RouteRefuse, d.Route, sql)
		assert.Equal(t, planner.ReasonDependentView, d.Reason, sql)
		assert.Equal(t, []string{`"app"."active_users"`, `"reports"."emails"`}, d.DependentViews, sql)
		assert.Equal(t, planner.RouteRefuse, plan.Route, sql)
	}

	for sql, reason := range map[string]planner.Reason{
		"ALTER TABLE t RENAME COLUMN email TO mail":       planner.ReasonAppBreakingRename,
		"ALTER TABLE t ALTER COLUMN email SET DEFAULT ''": planner.ReasonMetadataOnly,
		"ALTER TABLE t DROP COLUMN note":                  planner.ReasonMetadataOnly,
		"ALTER TABLE t ALTER COLUMN note TYPE text":       planner.ReasonBinaryCoercible,
	} {
		plan, err := planner.Classify(sql, views)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		assert.Equal(t, reason, plan.Decisions[0].Reason, sql)
		assert.Empty(t, plan.Decisions[0].DependentViews, sql)
	}

	plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN email TYPE varchar(100)",
		planner.Facts{ColumnTypes: views.ColumnTypes})
	require.NoError(t, err)
	assert.Equal(t, planner.ReasonBinaryCoercible, plan.Decisions[0].Reason)
}

// A refusal because views read the column calls each one by its kind and
// gives the manual path the change takes with them out of the way, the
// same one policies and triggers get: rewriting them before a drop,
// recreating them around a type change that runs in place, where a
// materialized view also runs its query to fill itself again, and moving
// them to a new column when the type change rewrites the table.
func TestClassifyNamesMaterializedViewsAndTheirManualPath(t *testing.T) {
	mail := `"reports"."mail"`
	const grants = "; a dropped view loses its grants, comment, owner, options such as security_barrier, rules, INSTEAD OF triggers and column defaults, and a materialized view its indexes too, so recreate those with each view"
	const redefine = "; changing a view means dropping and recreating it in one transaction, since CREATE OR REPLACE VIEW cannot remove or retype a column"
	facts := planner.Facts{
		ColumnTypes:        map[string]string{"email": "character varying(50)"},
		DependentViews:     map[string][]string{"email": {`"app"."active_users"`, mail}},
		DropDependentViews: map[string][]string{"email": {`"app"."active_users"`, mail}},
		MaterializedViews:  []string{mail},
	}
	for _, tc := range []struct {
		sql  string
		fix  planner.DependentFix
		help string
	}{
		{
			sql:  "ALTER TABLE t DROP COLUMN email",
			fix:  planner.DependentFixRewriteDependents,
			help: "change those views and materialized views so they no longer read the column, then re-plan the drop" + redefine + grants,
		},
		{
			sql: "ALTER TABLE t ALTER COLUMN email TYPE varchar(100)",
			fix: planner.DependentFixRecreateAroundChange,
			help: "this type change runs in place, without rewriting the table, so in one transaction drop those views and materialized views, run the change, and recreate them; " +
				"recreating a materialized view runs its query to fill it again while the transaction blocks the table" + grants,
		},
		{
			sql: "ALTER TABLE t ALTER COLUMN email TYPE bigint USING email::bigint",
			fix: planner.DependentFixExpandContract,
			help: "this type change rewrites the table, so recreating the views and materialized views around it would block the table for the whole rewrite; " +
				"add a column of the new type instead, have the application write to both and backfill it, " +
				"point those views and materialized views at it and switch reads to it, then drop the old column once nothing uses it; " +
				"pointing a view at the new column means dropping and recreating it, since CREATE OR REPLACE VIEW cannot remove or retype a column, " +
				"so do that in one transaction at that step" + grants,
		},
	} {
		plan, err := planner.Classify(tc.sql, facts)
		require.NoError(t, err, tc.sql)
		require.Len(t, plan.Decisions, 1, tc.sql)
		d := plan.Decisions[0]
		assert.Equal(t, planner.ReasonDependentView, d.Reason, tc.sql)
		assert.Equal(t, []string{mail}, d.DependentMaterializedViews, tc.sql)
		assert.Equal(t, tc.fix, d.DependentFix, tc.sql)
		assert.Equal(t, `view "app"."active_users" and materialized view "reports"."mail"`, d.ViewsPhrase(), tc.sql)
		assert.Equal(t, tc.help, d.ViewsHelp(), tc.sql)
	}

	for name, tc := range map[string]struct {
		d      planner.Decision
		phrase string
		noun   string
		grants string
	}{
		"views only": {
			d:      planner.Decision{DependentViews: []string{`"a"."v"`, `"a"."w"`}},
			phrase: `views "a"."v", "a"."w"`,
			noun:   "views",
			grants: "; a dropped view loses its grants, comment, owner, options such as security_barrier, rules, INSTEAD OF triggers and column defaults, so recreate those with each view",
		},
		"one materialized view": {
			d:      planner.Decision{DependentViews: []string{`"a"."m"`}, DependentMaterializedViews: []string{`"a"."m"`}},
			phrase: `materialized view "a"."m"`,
			noun:   "materialized views",
			grants: grants,
		},
	} {
		assert.Equal(t, tc.phrase, tc.d.ViewsPhrase(), name)
		assert.Equal(t, tc.noun, tc.d.ViewsNoun(), name)
		assert.Equal(t, "drop or redefine those "+tc.noun+" first, then re-plan"+redefine+tc.grants, tc.d.ViewsHelp(), name)
	}

	// A view that reads a blocking view, directly or through another,
	// has to be dropped before it, so the help names every view to drop,
	// deepest first, each by its kind, and to recreate them in reverse.
	facts.ViewReaders = map[string]map[string]int{
		mail:                   {`"reports"."mail_top"`: 1, `"reports"."mail_sum"`: 1, `"reports"."mail_top_n"`: 2},
		`"app"."active_users"`: {`"reports"."mail_top_n"`: 1},
	}
	facts.MaterializedViews = append(facts.MaterializedViews, `"reports"."mail_sum"`)
	plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN email TYPE varchar(100)", facts)
	require.NoError(t, err)
	d := plan.Decisions[0]
	assert.Equal(t, []string{`"reports"."mail_top_n"`, `"reports"."mail_sum"`, `"reports"."mail_top"`, `"app"."active_users"`, mail}, d.DependentViewDropOrder)
	assert.Equal(t, []string{mail, `"reports"."mail_sum"`}, d.DependentMaterializedViews)
	assert.Equal(t, "this type change runs in place, without rewriting the table, so in one transaction drop those views and materialized views, run the change, and recreate them; "+
		"recreating a materialized view runs its query to fill it again while the transaction blocks the table; "+
		`drop the views in this order: view "reports"."mail_top_n", then materialized view "reports"."mail_sum", then view "reports"."mail_top", `+
		`then view "app"."active_users", then materialized view "reports"."mail", and recreate them in reverse`+grants, d.ViewsHelp())
}

// When one blocking view reads another through a view that reads the
// first, the drop order puts each view ahead of every view it reads,
// blocking views included: v2 reads r, and r reads v1, so v2 is dropped
// first, then r, then v1, and PostgreSQL accepts each DROP VIEW.
func TestClassifyOrdersBlockingViewsThatReadEachOther(t *testing.T) {
	v1, v2, r := `"app"."v1"`, `"app"."v2"`, `"app"."r"`
	facts := planner.Facts{
		ColumnTypes:        map[string]string{"email": "character varying(50)"},
		DependentViews:     map[string][]string{"email": {v1, v2}},
		DropDependentViews: map[string][]string{"email": {v1, v2}},
		ViewReaders:        map[string]map[string]int{v1: {r: 1, v2: 2}},
	}
	plan, err := planner.Classify("ALTER TABLE t ALTER COLUMN email TYPE varchar(100)", facts)
	require.NoError(t, err)
	d := plan.Decisions[0]
	assert.Equal(t, []string{v2, r, v1}, d.DependentViewDropOrder)
	assert.Contains(t, d.ViewsHelp(), `drop the views in this order: view "app"."v2", then view "app"."r", then view "app"."v1", and recreate them in reverse`)
}

// A view on a table below counts against a recursive change and not
// against one under ONLY, which stops at the table. A recursive drop counts
// only the copies of the column it drops: a view on a child's own declared
// copy blocks a type change, which reaches that copy, and not a drop, which
// leaves it.
func TestClassifyJudgesDependentViewsByWhatTheChangeReaches(t *testing.T) {
	facts := planner.Facts{
		ColumnTypes: map[string]string{"email": "character varying(50)", "code": "text", "tag": "text"},
		DependentViews: map[string][]string{
			"email": {`"app"."active_users"`}, "code": {`"app"."child_codes"`}, "tag": {`"app"."child_tags"`},
		},
		DropDependentViews: map[string][]string{"email": {`"app"."active_users"`}, "code": {`"app"."child_codes"`}},
		OwnDependentViews:  map[string][]string{"email": {`"app"."active_users"`}},
	}
	for sql, want := range map[string][]string{
		"ALTER TABLE t DROP COLUMN email RESTRICT":         {`"app"."active_users"`},
		"ALTER TABLE ONLY t DROP COLUMN email":             {`"app"."active_users"`},
		"ALTER TABLE t DROP COLUMN code":                   {`"app"."child_codes"`},
		"ALTER TABLE t ALTER COLUMN code TYPE varchar(10)": {`"app"."child_codes"`},
	} {
		plan, err := planner.Classify(sql, facts)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		assert.Equal(t, planner.ReasonDependentView, plan.Decisions[0].Reason, sql)
		assert.Equal(t, want, plan.Decisions[0].DependentViews, sql)
	}

	plan, err := planner.Classify("ALTER TABLE ONLY t DROP COLUMN code", facts)
	require.NoError(t, err)
	assert.Equal(t, planner.ReasonMetadataOnly, plan.Decisions[0].Reason)

	plan, err = planner.Classify("ALTER TABLE t DROP COLUMN tag", facts)
	require.NoError(t, err)
	assert.Equal(t, planner.ReasonMetadataOnly, plan.Decisions[0].Reason)
	assert.Empty(t, plan.Decisions[0].DependentViews)

	plan, err = planner.Classify("ALTER TABLE t ALTER COLUMN tag TYPE varchar(10)", facts)
	require.NoError(t, err)
	assert.Equal(t, planner.ReasonDependentView, plan.Decisions[0].Reason)
	assert.Equal(t, []string{`"app"."child_tags"`}, plan.Decisions[0].DependentViews)
}

// PostgreSQL refuses to retype or drop a column a row-level security
// policy reads, so either change on such a column is refused with the
// policies named, even a type change that would otherwise relabel in
// place. The manual path follows what the change would do without the
// policies: a drop needs them rewritten, an in-place type change can run
// between dropping and recreating them, and a rewrite needs a new column.
// A rename, which the policy follows, and changes to columns no policy
// reads classify as usual.
func TestClassifyRefusesChangingAColumnAPolicyReads(t *testing.T) {
	policies := []string{`"own_rows" ON "public"."documents"`, `"team_rows" ON "public"."teams"`}
	facts := planner.Facts{
		ColumnTypes:           map[string]string{"owner_id": "character varying(36)", "label": "text"},
		DependentPolicies:     map[string][]string{"owner_id": policies},
		DropDependentPolicies: map[string][]string{"owner_id": policies},
		OwnDependentPolicies:  map[string][]string{"owner_id": policies},
	}
	for sql, fix := range map[string]planner.DependentFix{
		"ALTER TABLE documents ALTER COLUMN owner_id TYPE varchar(64)":               planner.DependentFixRecreateAroundChange,
		"ALTER TABLE documents ALTER COLUMN owner_id TYPE text":                      planner.DependentFixRecreateAroundChange,
		"ALTER TABLE documents ALTER COLUMN owner_id TYPE uuid USING owner_id::uuid": planner.DependentFixExpandContract,
		"ALTER TABLE documents DROP COLUMN owner_id":                                 planner.DependentFixRewriteDependents,
		"ALTER TABLE ONLY documents ALTER COLUMN owner_id TYPE text":                 planner.DependentFixRecreateAroundChange,
	} {
		plan, err := planner.Classify(sql, facts)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		d := plan.Decisions[0]
		assert.Equal(t, planner.RouteRefuse, d.Route, sql)
		assert.Equal(t, planner.ReasonDependentPolicy, d.Reason, sql)
		assert.Equal(t, policies, d.DependentPolicies, sql)
		assert.Equal(t, fix, d.DependentFix, sql)
		assert.Empty(t, d.DependentViews, sql)
		assert.Equal(t, planner.RouteRefuse, plan.Route, sql)
	}

	for sql, reason := range map[string]planner.Reason{
		"ALTER TABLE documents RENAME COLUMN owner_id TO owner": planner.ReasonAppBreakingRename,
		"ALTER TABLE documents DROP COLUMN label":               planner.ReasonMetadataOnly,
		"ALTER TABLE documents ALTER COLUMN label TYPE text":    planner.ReasonBinaryCoercible,
	} {
		plan, err := planner.Classify(sql, facts)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		assert.Equal(t, reason, plan.Decisions[0].Reason, sql)
		assert.Empty(t, plan.Decisions[0].DependentPolicies, sql)
	}
}

// PostgreSQL refuses to retype or drop a column a trigger's UPDATE OF list
// or WHEN condition names, even a type change that would otherwise relabel
// in place, so either change is refused with the triggers named. The
// manual path follows what the change would do without the triggers, as
// for a policy. A rename, which the trigger follows, classifies as usual.
func TestClassifyRefusesChangingAColumnATriggerNames(t *testing.T) {
	triggers := []string{`"audit_status" ON "public"."orders"`}
	facts := planner.Facts{
		ColumnTypes:       map[string]string{"status": "character varying(16)", "note": "text"},
		DependentTriggers: dependentsEverywhere("status", triggers),
	}
	for sql, fix := range map[string]planner.DependentFix{
		"ALTER TABLE orders ALTER COLUMN status TYPE varchar(64)":      planner.DependentFixRecreateAroundChange,
		"ALTER TABLE orders ALTER COLUMN status TYPE int USING 0":      planner.DependentFixExpandContract,
		"ALTER TABLE orders DROP COLUMN status":                        planner.DependentFixRewriteDependents,
		"ALTER TABLE ONLY orders ALTER COLUMN status TYPE varchar(64)": planner.DependentFixRecreateAroundChange,
	} {
		d := classifyWithFacts(t, sql, facts)
		assert.Equal(t, planner.RouteRefuse, d.Route, sql)
		assert.Equal(t, planner.ReasonDependentTrigger, d.Reason, sql)
		assert.Equal(t, triggers, d.DependentTriggers, sql)
		assert.Equal(t, fix, d.DependentFix, sql)
	}
	for sql, reason := range map[string]planner.Reason{
		"ALTER TABLE orders RENAME COLUMN status TO state":    planner.ReasonAppBreakingRename,
		"ALTER TABLE orders ALTER COLUMN note TYPE text":      planner.ReasonBinaryCoercible,
		"ALTER TABLE orders ALTER COLUMN status SET NOT NULL": planner.ReasonSaferIdiom,
	} {
		d := classifyWithFacts(t, sql, facts)
		assert.Equal(t, reason, d.Reason, sql)
		assert.Empty(t, d.DependentTriggers, sql)
	}
}

// PostgreSQL refuses to retype or drop a column a publication's column
// list or row filter names, so either change is refused with the
// publications named and a manual path that suits the change. A rename,
// which the publication follows, classifies as usual, and a column no
// publication names is not held back.
func TestClassifyRefusesChangingAColumnAPublicationNames(t *testing.T) {
	publications := []string{`"orders_pub"`}
	facts := planner.Facts{
		ColumnTypes:           map[string]string{"status": "character varying(16)", "note": "text"},
		DependentPublications: dependentsEverywhere("status", publications),
	}
	for _, sql := range []string{
		"ALTER TABLE orders ALTER COLUMN status TYPE varchar(64)",
		"ALTER TABLE orders ALTER COLUMN status TYPE int USING 0",
		"ALTER TABLE orders DROP COLUMN status",
		"ALTER TABLE ONLY orders DROP COLUMN status",
	} {
		d := classifyWithFacts(t, sql, facts)
		assert.Equal(t, planner.RouteRefuse, d.Route, sql)
		assert.Equal(t, planner.ReasonDependentPublication, d.Reason, sql)
		assert.Equal(t, publications, d.DependentPublications, sql)
	}
	for sql, reason := range map[string]planner.Reason{
		"ALTER TABLE orders RENAME COLUMN status TO state": planner.ReasonAppBreakingRename,
		"ALTER TABLE orders ALTER COLUMN note TYPE text":   planner.ReasonBinaryCoercible,
		"ALTER TABLE orders DROP COLUMN note":              planner.ReasonMetadataOnly,
	} {
		d := classifyWithFacts(t, sql, facts)
		assert.Equal(t, reason, d.Reason, sql)
		assert.Empty(t, d.DependentPublications, sql)
	}
	assert.Equal(t, "remove the column from those publications' column lists and row filters with ALTER PUBLICATION ... SET TABLE, then re-plan the drop",
		planner.PublicationHelp(statement.OpDropColumn))
	assert.Contains(t, planner.PublicationHelp(statement.OpAlterColumnType), "run the change, then add it back")
}

// A trigger and a publication both block a drop: the reason names the
// trigger, the kind checked first, and the publication is still named so
// the manual path clears both.
func TestClassifyNamesAPublicationBesideATrigger(t *testing.T) {
	facts := planner.Facts{
		ColumnTypes:           map[string]string{"status": "character varying(16)"},
		DependentTriggers:     dependentsEverywhere("status", []string{`"audit_status" ON "public"."orders"`}),
		DependentPublications: dependentsEverywhere("status", []string{`"orders_pub"`}),
	}
	d := classifyWithFacts(t, "ALTER TABLE orders DROP COLUMN status", facts)
	assert.Equal(t, planner.ReasonDependentTrigger, d.Reason)
	assert.Equal(t, []string{`"orders_pub"`}, d.DependentPublications)
	assert.Equal(t, `publication "orders_pub"`, d.OtherDependents())
}

// PostgreSQL refuses to retype or drop a column a generated column reads,
// so either change is refused with the generated columns named.
func TestClassifyRefusesChangingAColumnAGeneratedColumnReads(t *testing.T) {
	generated := []string{`"public"."orders"."total_cents"`}
	facts := planner.Facts{
		ColumnTypes:               map[string]string{"amount": "numeric(10,2)"},
		DependentGeneratedColumns: dependentsEverywhere("amount", generated),
	}
	for _, sql := range []string{
		"ALTER TABLE orders ALTER COLUMN amount TYPE numeric(12,2)",
		"ALTER TABLE orders DROP COLUMN amount",
	} {
		d := classifyWithFacts(t, sql, facts)
		assert.Equal(t, planner.RouteRefuse, d.Route, sql)
		assert.Equal(t, planner.ReasonDependentGeneratedColumn, d.Reason, sql)
		assert.Equal(t, generated, d.DependentGeneratedColumns, sql)
	}
}

// PostgreSQL runs every drop of an ALTER TABLE before its type changes,
// wherever the drop is written, so a generated column the same statement
// drops does not block widening the column it reads. A copy the drop does
// not reach, such as a child's under ONLY or one the child declares
// itself, still blocks it.
func TestClassifyDiscountsAGeneratedColumnTheStatementDrops(t *testing.T) {
	own := `"public"."orders"."total"`
	child := `"public"."orders_archive"."total"`
	facts := planner.Facts{
		ColumnTypes: map[string]string{"amount": "numeric(10,2)", "total": "numeric"},
		DependentGeneratedColumns: planner.ColumnDependents{
			Tree: map[string][]string{"amount": {child, own}},
			Drop: map[string][]string{"amount": {child, own}},
			Own:  map[string][]string{"amount": {own}},
		},
		GeneratedColumnCopies: planner.ColumnDependents{
			Tree: map[string][]string{"total": {child, own}},
			Drop: map[string][]string{"total": {child, own}},
			Own:  map[string][]string{"total": {own}},
		},
	}
	for _, sql := range []string{
		"ALTER TABLE orders DROP COLUMN total, ALTER COLUMN amount TYPE numeric(12,2)",
		"ALTER TABLE orders ALTER COLUMN amount TYPE numeric(12,2), DROP COLUMN total",
	} {
		plan, err := planner.Classify(sql, facts)
		require.NoError(t, err, sql)
		assert.Equal(t, planner.RouteNative, plan.Route, sql)
		require.Len(t, plan.Decisions, 2, sql)
		for _, d := range plan.Decisions {
			assert.Empty(t, d.DependentGeneratedColumns, sql)
		}
	}

	plan, err := planner.Classify("ALTER TABLE ONLY orders DROP COLUMN total, ALTER COLUMN amount TYPE numeric(12,2)", facts)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 2)
	assert.Equal(t, planner.RouteNative, plan.Decisions[1].Route, "ONLY reaches the table's own generated column alone")

	declared := facts
	declared.GeneratedColumnCopies.Drop = map[string][]string{"total": {own}}
	plan, err = planner.Classify("ALTER TABLE orders DROP COLUMN total, ALTER COLUMN amount TYPE numeric(12,2)", declared)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 2)
	assert.Equal(t, planner.ReasonDependentGeneratedColumn, plan.Decisions[1].Reason)
	assert.Equal(t, []string{child}, plan.Decisions[1].DependentGeneratedColumns,
		"the child declares its own copy, which the drop leaves")

	after, err := planner.FactsAfter("ALTER TABLE orders DROP COLUMN total", facts)
	require.NoError(t, err)
	assert.Equal(t, planner.ColumnDependents{Tree: map[string][]string{}, Drop: map[string][]string{}, Own: map[string][]string{}},
		after.DependentGeneratedColumns)
	assert.Equal(t, []string{child, own}, facts.DependentGeneratedColumns.Tree["amount"], "the caller's facts are left as they were")
	d := classifyWithFacts(t, "ALTER TABLE orders ALTER COLUMN amount TYPE numeric(12,2)", after)
	assert.Equal(t, planner.ReasonBinaryCoercible, d.Reason, "a later statement sees the drop")
}

// The drops of one ALTER TABLE run in the order written, so a drop of a
// column a generated column reads is refused unless the same statement
// drops the generated column first. Every other operation runs after all
// the drops, so a type change sees the generated column gone wherever its
// drop is written.
func TestClassifyJudgesADropByTheDropsWrittenBeforeIt(t *testing.T) {
	total := `"public"."ledger"."total"`
	facts := planner.Facts{
		ColumnTypes:               map[string]string{"amount": "integer", "total": "integer"},
		DependentGeneratedColumns: dependentsEverywhere("amount", []string{total}),
		GeneratedColumnCopies:     dependentsEverywhere("total", []string{total}),
	}

	plan, err := planner.Classify("ALTER TABLE ledger DROP COLUMN amount, DROP COLUMN total", facts)
	require.NoError(t, err)
	assert.Equal(t, planner.RouteRefuse, plan.Route)
	require.Len(t, plan.Decisions, 2)
	assert.Equal(t, planner.ReasonDependentGeneratedColumn, plan.Decisions[0].Reason)
	assert.Equal(t, []string{total}, plan.Decisions[0].DependentGeneratedColumns)
	assert.Equal(t, planner.ReasonMetadataOnly, plan.Decisions[1].Reason)

	plan, err = planner.Classify("ALTER TABLE ledger DROP COLUMN total, DROP COLUMN amount", facts)
	require.NoError(t, err)
	assert.Equal(t, planner.RouteNative, plan.Route)
	require.Len(t, plan.Decisions, 2)
	for _, d := range plan.Decisions {
		assert.Equal(t, planner.ReasonMetadataOnly, d.Reason)
		assert.Empty(t, d.DependentGeneratedColumns)
	}
}

// A partition's copy of a generated column goes when the parent's copy is
// dropped, and PostgreSQL refuses to drop it on its own, so a refusal
// names only the parent's copy. A copy a child declares itself survives
// the parent's drop and is named too.
func TestClassifyNamesOnlyTheGeneratedColumnCopiesToDrop(t *testing.T) {
	own := `"public"."events"."total"`
	partition := `"public"."events_2026"."total"`
	declared := `"public"."events_legacy"."total"`
	facts := planner.Facts{
		ColumnTypes: map[string]string{"amount": "integer"},
		DependentGeneratedColumns: planner.ColumnDependents{
			Tree: map[string][]string{"amount": {partition, declared, own}},
			Drop: map[string][]string{"amount": {partition, own}},
			Own:  map[string][]string{"amount": {own}},
		},
		GeneratedColumnCopies: planner.ColumnDependents{
			Tree: map[string][]string{"total": {partition, declared, own}},
			Drop: map[string][]string{"total": {partition, own}},
			Own:  map[string][]string{"total": {own}},
		},
	}
	d := classifyWithFacts(t, "ALTER TABLE events ALTER COLUMN amount TYPE bigint", facts)
	assert.Equal(t, planner.ReasonDependentGeneratedColumn, d.Reason)
	assert.Equal(t, []string{declared, own}, d.DependentGeneratedColumns)

	d = classifyWithFacts(t, "ALTER TABLE events DROP COLUMN amount", facts)
	assert.Equal(t, []string{own}, d.DependentGeneratedColumns)
}

// A change several kinds of dependent block names every one of them, so
// the manual path clears them all; the reason is the first kind.
func TestClassifyNamesEveryKindOfDependentThatBlocks(t *testing.T) {
	policy := []string{`"open_only" ON "public"."orders"`}
	trigger := []string{`"audit_status" ON "public"."orders"`}
	generated := []string{`"public"."orders"."status_upper"`}
	facts := planner.Facts{
		ColumnTypes:               map[string]string{"status": "character varying(16)"},
		DependentPolicies:         map[string][]string{"status": policy},
		DependentTriggers:         dependentsEverywhere("status", trigger),
		DependentGeneratedColumns: dependentsEverywhere("status", generated),
	}
	d := classifyWithFacts(t, "ALTER TABLE orders ALTER COLUMN status TYPE varchar(64)", facts)
	assert.Equal(t, planner.ReasonDependentPolicy, d.Reason)
	assert.Equal(t, policy, d.DependentPolicies)
	assert.Equal(t, trigger, d.DependentTriggers)
	assert.Equal(t, generated, d.DependentGeneratedColumns)
	assert.Equal(t, `trigger "audit_status" ON "public"."orders" and generated column "public"."orders"."status_upper"`, d.OtherDependents())

	facts.DependentPolicies, facts.DependentGeneratedColumns = nil, planner.ColumnDependents{}
	d = classifyWithFacts(t, "ALTER TABLE orders ALTER COLUMN status TYPE varchar(64)", facts)
	assert.Equal(t, planner.ReasonDependentTrigger, d.Reason)
	assert.Empty(t, d.OtherDependents())
}

// The manual path for a type change dependents block follows what the
// conversion itself costs, so a widening a policy and a trigger both
// block, or a trigger and a generated column, is still recreated around
// rather than routed through a new column.
func TestClassifyJudgesTheDependentFixByTheConversionAlone(t *testing.T) {
	facts := planner.Facts{
		ColumnTypes:               map[string]string{"status": "character varying(16)"},
		DependentPolicies:         map[string][]string{"status": {`"open_only" ON "public"."orders"`}},
		DependentTriggers:         dependentsEverywhere("status", []string{`"audit_status" ON "public"."orders"`}),
		DependentGeneratedColumns: dependentsEverywhere("status", []string{`"public"."orders"."status_upper"`}),
	}
	d := classifyWithFacts(t, "ALTER TABLE orders ALTER COLUMN status TYPE varchar(64)", facts)
	assert.Equal(t, planner.ReasonDependentPolicy, d.Reason)
	assert.Equal(t, planner.DependentFixRecreateAroundChange, d.DependentFix)

	facts.DependentPolicies = nil
	d = classifyWithFacts(t, "ALTER TABLE orders ALTER COLUMN status TYPE varchar(64)", facts)
	assert.Equal(t, planner.ReasonDependentTrigger, d.Reason)
	assert.Equal(t, planner.DependentFixRecreateAroundChange, d.DependentFix)

	d = classifyWithFacts(t, "ALTER TABLE orders ALTER COLUMN status TYPE int USING 0", facts)
	assert.Equal(t, planner.DependentFixExpandContract, d.DependentFix)
}

// PostgreSQL never retypes or drops a partition key column, so either
// change is refused with the partitioned tables named. The partition key
// is judged before every other dependent, since no object can be moved
// aside to let the change run, and by what the change reaches: a
// sub-partition's key counts against a recursive change and not against
// one under ONLY.
func TestClassifyRefusesChangingAPartitionKeyColumn(t *testing.T) {
	parent := []string{`"public"."events"`}
	sub := []string{`"public"."events_2026"`}
	facts := planner.Facts{
		ColumnTypes: map[string]string{"created_at": "timestamp without time zone", "region": "character varying(8)"},
		PartitionKeys: planner.ColumnDependents{
			Tree: map[string][]string{"created_at": parent, "region": sub},
			Drop: map[string][]string{"created_at": parent, "region": sub},
			Own:  map[string][]string{"created_at": parent},
		},
		DependentViews:    map[string][]string{"created_at": {`"public"."recent_events"`}},
		DependentTriggers: dependentsEverywhere("created_at", []string{`"stamp" ON "public"."events"`}),
	}
	for sql, want := range map[string][]string{
		"ALTER TABLE events ALTER COLUMN created_at TYPE timestamptz": parent,
		"ALTER TABLE events DROP COLUMN created_at":                   parent,
		"ALTER TABLE events ALTER COLUMN region TYPE varchar(16)":     sub,
		"ALTER TABLE events DROP COLUMN region":                       sub,
	} {
		d := classifyWithFacts(t, sql, facts)
		assert.Equal(t, planner.RouteRefuse, d.Route, sql)
		assert.Equal(t, planner.ReasonPartitionKey, d.Reason, sql)
		assert.Equal(t, want, d.PartitionKeyTables, sql)
		assert.Empty(t, d.DependentViews, sql)
		assert.Empty(t, d.DependentTriggers, sql)
	}
	d := classifyWithFacts(t, "ALTER TABLE ONLY events ALTER COLUMN region TYPE varchar(16)", facts)
	assert.Equal(t, planner.ReasonBinaryCoercible, d.Reason)
	assert.Empty(t, d.PartitionKeyTables)
}

// dependentsEverywhere is a column's dependents as every change to it
// reaches them: recursive, recursive drop, and ONLY alike.
func dependentsEverywhere(column string, names []string) planner.ColumnDependents {
	m := map[string][]string{column: names}
	return planner.ColumnDependents{Tree: m, Drop: m, Own: m}
}

// classifyWithFacts classifies a single-operation statement against facts
// and returns its decision.
func classifyWithFacts(t *testing.T, sql string, facts planner.Facts) planner.Decision {
	t.Helper()
	plan, err := planner.Classify(sql, facts)
	require.NoError(t, err, sql)
	require.Len(t, plan.Decisions, 1, sql)
	return plan.Decisions[0]
}

// A policy on a partition or inheritance child counts against a
// recursive change and not against one under ONLY, and a recursive drop
// counts only the copies of the column it drops, the same reach the
// dependent-view refusal judges by. A column both a view and a policy read
// is refused for the view first, with the policy named as well.
func TestClassifyJudgesDependentPoliciesByWhatTheChangeReaches(t *testing.T) {
	childPolicy := []string{`"child_rows" ON "app"."documents_child"`}
	facts := planner.Facts{
		ColumnTypes:           map[string]string{"owner_id": "character varying(36)", "tag": "text", "email": "text"},
		DependentPolicies:     map[string][]string{"owner_id": childPolicy, "tag": childPolicy, "email": childPolicy},
		DropDependentPolicies: map[string][]string{"owner_id": childPolicy, "email": childPolicy},
		DependentViews:        map[string][]string{"email": {`"app"."emails"`}},
		DropDependentViews:    map[string][]string{"email": {`"app"."emails"`}},
	}
	for sql, want := range map[string]planner.Reason{
		"ALTER TABLE documents ALTER COLUMN owner_id TYPE text":      planner.ReasonDependentPolicy,
		"ALTER TABLE ONLY documents ALTER COLUMN owner_id TYPE text": planner.ReasonBinaryCoercible,
		"ALTER TABLE documents DROP COLUMN owner_id":                 planner.ReasonDependentPolicy,
		"ALTER TABLE documents DROP COLUMN tag":                      planner.ReasonMetadataOnly,
		"ALTER TABLE documents ALTER COLUMN tag TYPE varchar(10)":    planner.ReasonDependentPolicy,
		"ALTER TABLE documents DROP COLUMN email":                    planner.ReasonDependentView,
	} {
		plan, err := planner.Classify(sql, facts)
		require.NoError(t, err, sql)
		require.Len(t, plan.Decisions, 1, sql)
		assert.Equal(t, want, plan.Decisions[0].Reason, sql)
		if want == planner.ReasonDependentPolicy || want == planner.ReasonDependentView {
			assert.Equal(t, childPolicy, plan.Decisions[0].DependentPolicies, sql)
		} else {
			assert.Empty(t, plan.Decisions[0].DependentPolicies, sql)
		}
	}
}

// CASCADE also drops whatever depends on the target, objects the
// statement does not name, so every drop with it is refused, from the
// statement alone and whatever the live facts say, while the default
// RESTRICT classifies as usual. A drop the replica identity guard refuses
// keeps that more specific reason.
func TestClassifyRefusesACascadingDrop(t *testing.T) {
	views := planner.Facts{
		ColumnTypes:        map[string]string{"email": "character varying(50)"},
		DependentViews:     map[string][]string{"email": {`"app"."active_users"`}},
		DropDependentViews: map[string][]string{"email": {`"app"."active_users"`}},
		OwnDependentViews:  map[string][]string{"email": {`"app"."active_users"`}},
	}
	for _, facts := range []planner.Facts{{}, views} {
		for _, sql := range []string{
			"ALTER TABLE t DROP COLUMN email CASCADE",
			"ALTER TABLE ONLY t DROP COLUMN email CASCADE",
			"ALTER TABLE t DROP COLUMN IF EXISTS email CASCADE",
			"ALTER TABLE t DROP CONSTRAINT t_pkey CASCADE",
			"ALTER TABLE t DROP CONSTRAINT IF EXISTS t_pkey CASCADE",
			"DROP INDEX t_email_idx CASCADE",
			"DROP INDEX app.t_email_idx, app.t_other_idx CASCADE",
		} {
			plan, err := planner.Classify(sql, facts)
			require.NoError(t, err, sql)
			require.Len(t, plan.Decisions, 1, sql)
			d := plan.Decisions[0]
			assert.Equal(t, planner.RouteRefuse, d.Route, sql)
			assert.Equal(t, planner.ReasonCascadingDrop, d.Reason, sql)
			assert.True(t, d.Destructive, sql)
			assert.Empty(t, d.SaferSQL, "DROP INDEX CONCURRENTLY does not accept CASCADE: %s", sql)
			assert.Empty(t, d.DependentViews, sql)
			assert.Equal(t, planner.RouteRefuse, plan.Route, sql)
		}
	}

	plan, err := planner.Classify("ALTER TABLE t ADD COLUMN note text, DROP CONSTRAINT t_pkey CASCADE", planner.Facts{})
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 2)
	assert.Equal(t, planner.ReasonMetadataOnly, plan.Decisions[0].Reason)
	assert.Equal(t, planner.ReasonCascadingDrop, plan.Decisions[1].Reason)
	assert.Equal(t, planner.RouteRefuse, plan.Route)

	for sql, reason := range map[string]planner.Reason{
		"ALTER TABLE t DROP CONSTRAINT t_pkey":          planner.ReasonMetadataOnly,
		"ALTER TABLE t DROP CONSTRAINT t_pkey RESTRICT": planner.ReasonMetadataOnly,
		"DROP INDEX t_email_idx RESTRICT":               planner.ReasonSaferIdiom,
	} {
		plan, err := planner.Classify(sql, planner.Facts{})
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
		{"add unnamed check", "ALTER TABLE t ADD CHECK (age > 0)", planner.RouteNative, planner.ReasonSaferIdiom, 2},
		{"add unnamed foreign key", "ALTER TABLE t ADD FOREIGN KEY (pid) REFERENCES p (id)", planner.RouteNative, planner.ReasonSaferIdiom, 2},
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

// A helper an earlier run of the sequence left on the live table is
// validated, not added again: the sequence resumes at its validation, and
// its last step still drops it.
func TestClassifySetNotNullSequenceResumesAtALiveHelper(t *testing.T) {
	withHelper := facts
	withHelper.NotNullHelpers = map[string]bool{"age": true}
	plan, err := planner.Classify("ALTER TABLE s.t ALTER COLUMN age SET NOT NULL", withHelper)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	d := plan.Decisions[0]
	assert.Equal(t, planner.RouteNative, d.Route)
	assert.Equal(t, []string{
		`ALTER TABLE "s"."t" VALIDATE CONSTRAINT "t_age_not_null_check"`,
		`ALTER TABLE "s"."t" ALTER COLUMN "age" SET NOT NULL`,
		`ALTER TABLE "s"."t" DROP CONSTRAINT "t_age_not_null_check"`,
	}, d.SaferSQL)

	other, err := planner.Classify("ALTER TABLE s.t ALTER COLUMN name SET NOT NULL", withHelper)
	require.NoError(t, err)
	assert.Len(t, other.Decisions[0].SaferSQL, 4, "another column's helper is added as usual")
}

func TestClassifyAddPrimaryKeySequence(t *testing.T) {
	d := classifyOne(t, "ALTER TABLE s.t ADD CONSTRAINT t_pkey PRIMARY KEY (id)")
	assert.Equal(t, []string{
		`CREATE UNIQUE INDEX CONCURRENTLY "t_pkey" ON "s"."t" ("id")`,
		`ALTER TABLE "s"."t" ADD CONSTRAINT "t_pkey" PRIMARY KEY USING INDEX "t_pkey"`,
	}, d.SaferSQL)
}

// The online sequence must declare the constraint the statement declares:
// an index built without INCLUDE, NULLS NOT DISTINCT, or the tablespace, or
// an attach without DEFERRABLE or INITIALLY DEFERRED, is a different
// constraint. WITH (...) storage parameters ride on the index too; only a
// parameter the sequence cannot render faithfully gets no sequence, so the
// decision carries guidance instead of SQL that drops it.
// TestUsingIndexSequenceDeclaresTheSameConstraint proves each on a server.
func TestClassifyAddKeySequenceCarriesEveryAttribute(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			"include columns",
			"ALTER TABLE t ADD CONSTRAINT k UNIQUE (email) INCLUDE (name, id)",
			[]string{
				`CREATE UNIQUE INDEX CONCURRENTLY "k" ON "t" ("email") INCLUDE ("name", "id")`,
				`ALTER TABLE "t" ADD CONSTRAINT "k" UNIQUE USING INDEX "k"`,
			},
		},
		{
			"nulls not distinct",
			"ALTER TABLE t ADD CONSTRAINT k UNIQUE NULLS NOT DISTINCT (email)",
			[]string{
				`CREATE UNIQUE INDEX CONCURRENTLY "k" ON "t" ("email") NULLS NOT DISTINCT`,
				`ALTER TABLE "t" ADD CONSTRAINT "k" UNIQUE USING INDEX "k"`,
			},
		},
		{
			"index tablespace",
			"ALTER TABLE t ADD CONSTRAINT k UNIQUE (email) USING INDEX TABLESPACE fast",
			[]string{
				`CREATE UNIQUE INDEX CONCURRENTLY "k" ON "t" ("email") TABLESPACE "fast"`,
				`ALTER TABLE "t" ADD CONSTRAINT "k" UNIQUE USING INDEX "k"`,
			},
		},
		{
			"storage parameters",
			"ALTER TABLE t ADD CONSTRAINT k UNIQUE (email) INCLUDE (id) WITH (fillfactor = 70, deduplicate_items = off) USING INDEX TABLESPACE fast",
			[]string{
				`CREATE UNIQUE INDEX CONCURRENTLY "k" ON "t" ("email") INCLUDE ("id") WITH ("fillfactor" = '70', "deduplicate_items" = 'off') TABLESPACE "fast"`,
				`ALTER TABLE "t" ADD CONSTRAINT "k" UNIQUE USING INDEX "k"`,
			},
		},
		{
			"deferrable initially deferred",
			"ALTER TABLE t ADD CONSTRAINT k UNIQUE (email) DEFERRABLE INITIALLY DEFERRED",
			[]string{
				`CREATE UNIQUE INDEX CONCURRENTLY "k" ON "t" ("email")`,
				`ALTER TABLE "t" ADD CONSTRAINT "k" UNIQUE USING INDEX "k" DEFERRABLE INITIALLY DEFERRED`,
			},
		},
		{
			"deferrable primary key",
			"ALTER TABLE t ADD CONSTRAINT t_pkey PRIMARY KEY (id) DEFERRABLE",
			[]string{
				`CREATE UNIQUE INDEX CONCURRENTLY "t_pkey" ON "t" ("id")`,
				`ALTER TABLE "t" ADD CONSTRAINT "t_pkey" PRIMARY KEY USING INDEX "t_pkey" DEFERRABLE`,
			},
		},
		{
			"every attribute at once",
			"ALTER TABLE t ADD CONSTRAINT k UNIQUE NULLS NOT DISTINCT (a, b) INCLUDE (c) USING INDEX TABLESPACE fast DEFERRABLE INITIALLY DEFERRED",
			[]string{
				`CREATE UNIQUE INDEX CONCURRENTLY "k" ON "t" ("a", "b") INCLUDE ("c") NULLS NOT DISTINCT TABLESPACE "fast"`,
				`ALTER TABLE "t" ADD CONSTRAINT "k" UNIQUE USING INDEX "k" DEFERRABLE INITIALLY DEFERRED`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyOne(t, tc.sql).SaferSQL)
		})
	}

	d := classifyOne(t, "ALTER TABLE t ADD CONSTRAINT k UNIQUE (email) WITH (fillfactor = s.seventy)")
	assert.Equal(t, planner.RouteNative, d.Route)
	assert.Equal(t, planner.ReasonSaferIdiom, d.Reason)
	assert.Empty(t, d.SaferSQL, "a sequence would build the index without the parameter it cannot carry")
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

// Adding a NOT NULL column is refused when the server proved a row of the
// table would hold NULL in it (Facts.NullFilledColumns); on an empty
// table, or without the fact, it classifies as usual.
func TestClassifyRefusesANotNullColumnNothingFillsOnATableWithRows(t *testing.T) {
	populated := planner.Facts{
		NullFilledColumns: map[string]bool{"tier": true},
		ColumnTypes:       map[string]string{"id": "bigint", "email": "text"},
		AddColumnProofs:   map[string]statement.AddColumnProof{"tier": statement.AddColumnInPlace},
	}
	d := classifyWithFacts(t, "ALTER TABLE customers ADD COLUMN tier text NOT NULL", populated)
	assert.Equal(t, planner.ReasonNotNullWithoutDefault, d.Reason)
	assert.Equal(t, planner.RouteRefuse, d.Route)

	empty := populated
	empty.NullFilledColumns = nil
	d = classifyWithFacts(t, "ALTER TABLE customers ADD COLUMN tier text NOT NULL", empty)
	assert.Equal(t, planner.ReasonMetadataOnly, d.Reason)
	assert.Equal(t, planner.RouteNative, d.Route)
}

// Adding a column is refused when the table has no column number left
// (Facts.ColumnLimitReached), ahead of every other way the add could run;
// without the fact it classifies as usual.
func TestClassifyRefusesAColumnAddPastTheColumnLimit(t *testing.T) {
	numbers := preflight.ColumnNumbers{Fullest: []preflight.FullTable{{Name: `"app"."events"`, Root: true}}}
	full := planner.Facts{
		ColumnLimitReached: map[string]bool{"kind": true},
		ColumnNumbers:      numbers,
		ColumnTypes:        map[string]string{"id": "bigint"},
		AddColumnProofs:    map[string]statement.AddColumnProof{"kind": statement.AddColumnInPlace},
	}
	d := classifyWithFacts(t, "ALTER TABLE events ADD COLUMN kind text", full)
	assert.Equal(t, planner.ReasonColumnLimit, d.Reason)
	assert.Equal(t, planner.RouteRefuse, d.Route)
	assert.Equal(t, &numbers, d.ColumnLimit, "the decision carries the read that names the tables at the limit")

	roomy := full
	roomy.ColumnLimitReached = nil
	d = classifyWithFacts(t, "ALTER TABLE events ADD COLUMN kind text", roomy)
	assert.Equal(t, planner.ReasonMetadataOnly, d.Reason)
	assert.Nil(t, d.ColumnLimit)
}

// The column-limit refusal names the tables that have used every number,
// the table itself or the partitions or inheritance children below it, and
// gives the rebuild for each. When the table has numbers left, the subject
// and the offer to add fewer columns both say how many.
func TestColumnLimitSubjectAndHelp(t *testing.T) {
	const givesBack = "which gives back the numbers its dropped columns hold (VACUUM FULL does not)"
	for name, tc := range map[string]struct {
		numbers *preflight.ColumnNumbers
		subject string
		help    string
	}{
		"no read": {
			subject: "a table that has used all 1600 of PostgreSQL's column numbers",
			help:    "copy the table into a new one, " + givesBack,
		},
		"the table itself, none free": {
			numbers: &preflight.ColumnNumbers{Fullest: []preflight.FullTable{{Name: `"app"."events"`, Root: true}}},
			subject: "a table that has used all 1600 of PostgreSQL's column numbers",
			help:    "copy the table into a new one, " + givesBack + "; until then no column number is free",
		},
		"the table itself, one free": {
			numbers: &preflight.ColumnNumbers{Left: 1, Fullest: []preflight.FullTable{{Name: `"app"."events"`, Root: true}}},
			subject: "a table that has used all but 1 of PostgreSQL's 1600 column numbers",
			help:    "copy the table into a new one, " + givesBack + ", or add at most 1 column",
		},
		"a partition, none free": {
			numbers: &preflight.ColumnNumbers{Fullest: []preflight.FullTable{{Name: `"app"."ledger_1"`, Partition: true}}},
			subject: `a table whose partition "app"."ledger_1" has used all 1600 of PostgreSQL's column numbers`,
			help: `copy partition "app"."ledger_1" into a new table, ` + givesBack +
				", then detach the partition and attach the copy in its place; until then no column number is free",
		},
		"an inheritance child, three free": {
			numbers: &preflight.ColumnNumbers{Left: 3, Fullest: []preflight.FullTable{{Name: `"app"."child"`}}},
			subject: `a table whose inheritance child "app"."child" has used all but 3 of PostgreSQL's 1600 column numbers`,
			help: `copy inheritance child "app"."child" into a new table, ` + givesBack +
				", then make the copy inherit from the same parent in its place, or add at most 3 columns",
		},
		"two partitions": {
			numbers: &preflight.ColumnNumbers{Fullest: []preflight.FullTable{
				{Name: `"app"."ledger_1"`, Partition: true}, {Name: `"app"."ledger_2"`, Partition: true},
			}},
			subject: `a table whose partitions "app"."ledger_1", "app"."ledger_2" have used all 1600 of PostgreSQL's column numbers`,
			help: `copy each of the partitions "app"."ledger_1", "app"."ledger_2" into a new table, ` + givesBack +
				", then put each copy in its table's place: detach a partition and attach its copy, or make a child's copy inherit from the same parent" +
				"; until then no column number is free",
		},
		"the table and a partition": {
			numbers: &preflight.ColumnNumbers{Fullest: []preflight.FullTable{
				{Name: `"app"."ledger"`, Root: true}, {Name: `"app"."ledger_1"`, Partition: true},
			}},
			subject: `a table that has used all 1600 of PostgreSQL's column numbers, as have its partition "app"."ledger_1"`,
			help:    "copy the table into a new one, " + givesBack + `, and copy its partition "app"."ledger_1" the same way; until then no column number is free`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.subject, planner.ColumnLimitSubject(tc.numbers))
			assert.Equal(t, tc.help, planner.ColumnLimitHelp(tc.numbers))
		})
	}
}

// Each new column takes one of the numbers left, in statement order, and the
// ones added after the last number is gone are past the limit. A column the
// table already has takes none, a re-added dropped column takes a new one,
// and a drop gives none back.
func TestColumnsPastLimit(t *testing.T) {
	ops := func(sql string) []statement.Op {
		t.Helper()
		parsed, err := statement.ParseOps(sql)
		require.NoError(t, err)
		return parsed
	}
	live := map[string]string{"id": "bigint", "email": "text"}

	past, left := planner.ColumnsPastLimit(ops("ALTER TABLE events ADD COLUMN a int, ADD COLUMN b int, ADD COLUMN c int"), live, 2)
	assert.Equal(t, map[string]bool{"c": true}, past)
	assert.Zero(t, left)

	past, left = planner.ColumnsPastLimit(ops("ALTER TABLE events ADD COLUMN a int"), live, 3)
	assert.Nil(t, past)
	assert.Equal(t, 2, left)

	past, left = planner.ColumnsPastLimit(ops("ALTER TABLE events ADD COLUMN IF NOT EXISTS email text"), live, 0)
	assert.Nil(t, past, "a column the table already has takes no number")
	assert.Zero(t, left)

	past, _ = planner.ColumnsPastLimit(ops("ALTER TABLE events DROP COLUMN email, ADD COLUMN email text"), live, 0)
	assert.Equal(t, map[string]bool{"email": true}, past, "a re-added column takes a new number, and the drop gives none back")

	past, _ = planner.ColumnsPastLimit(ops("ALTER TABLE events DROP COLUMN email"), live, 0)
	assert.Nil(t, past)
}

// The columns whose rows may be left NULL are the ones added with no
// DEFAULT of their own that the table does not already have, NOT NULL or
// not, since a domain or an inline PRIMARY KEY can make a column NOT NULL
// the statement never spells. A constant, expression, identity, or serial
// default and a generated column fill the rows; a column the table has is
// skipped by ADD COLUMN IF NOT EXISTS, unless the same statement drops it
// first.
func TestNullFillCandidates(t *testing.T) {
	columns := map[string]string{"id": "bigint", "email": "text"}
	for sql, want := range map[string][]string{
		"ALTER TABLE customers ADD COLUMN tier text NOT NULL":                                   {"tier"},
		"ALTER TABLE customers ADD COLUMN tier tier_type NOT NULL":                              {"tier"},
		"ALTER TABLE customers ADD COLUMN tier text NOT NULL, ADD COLUMN plan text NOT NULL":    {"tier", "plan"},
		"ALTER TABLE customers ADD COLUMN tier text NOT NULL DEFAULT 'free'":                    nil,
		"ALTER TABLE customers ADD COLUMN created timestamptz NOT NULL DEFAULT now()":           nil,
		"ALTER TABLE customers ADD COLUMN n bigint NOT NULL GENERATED ALWAYS AS IDENTITY":       nil,
		"ALTER TABLE customers ADD COLUMN n bigserial NOT NULL":                                 nil,
		"ALTER TABLE customers ADD COLUMN total int NOT NULL GENERATED ALWAYS AS (1) STORED":    nil,
		"ALTER TABLE customers ADD COLUMN tier text":                                            {"tier"},
		"ALTER TABLE customers ADD COLUMN tier required_text":                                   {"tier"},
		"ALTER TABLE customers ADD COLUMN n bigint PRIMARY KEY":                                 {"n"},
		"ALTER TABLE customers ADD COLUMN n bigint PRIMARY KEY DEFAULT 0":                       nil,
		"ALTER TABLE customers ADD COLUMN IF NOT EXISTS email text NOT NULL":                    nil,
		"ALTER TABLE customers DROP COLUMN email, ADD COLUMN email text NOT NULL":               {"email"},
		"ALTER TABLE customers ADD COLUMN IF NOT EXISTS email text NOT NULL, DROP COLUMN email": nil,
	} {
		ops, err := statement.ParseOps(sql)
		require.NoError(t, err, sql)
		assert.Equal(t, want, planner.NullFillCandidates(ops, columns), sql)
	}
}

// An added column classifies by what the server proved about the rewrite
// before the statement's shape: a constant default on a constrained domain
// still rewrites, and a default the catalog stores runs in place however it
// is written. A proven rewrite takes its reason from what the server found
// rewrites, the type or the default, never from how the default is
// written. A column the server could not prove, or that no proof was run
// for, keeps the shape rules, and an expression default there fails closed
// as unverified. Only a column with no default or a literal one, proven
// to add in place without reading rows or running user code, is
// catalog-only: an expression default is evaluated under the lock, with
// the session's settings, so the probe's answer does not bound its cost.
func TestClassifyAddColumnByTheProvenRewrite(t *testing.T) {
	proven := planner.Facts{AddColumnProofs: map[string]statement.AddColumnProof{
		"code":     statement.AddColumnTypeRewrites,
		"strict":   statement.AddColumnTypeRewrites,
		"scored":   statement.AddColumnTypeRewrites,
		"id":       statement.AddColumnDefaultRewrites,
		"created":  statement.AddColumnInPlace,
		"checked":  statement.AddColumnInPlace,
		"age":      statement.AddColumnInPlace,
		"filled":   statement.AddColumnInPlaceScans,
		"blank":    statement.AddColumnInPlaceScans,
		"tier":     statement.AddColumnInPlace,
		"inherits": statement.AddColumnInPlaceRunsUserCode,
		"once":     statement.AddColumnInPlaceRunsUserCode,
		"scope":    statement.AddColumnInPlaceRunsUserCode,
		"high":     statement.AddColumnUnproven,
		"capped":   statement.AddColumnUnproven,
	}}
	cases := []struct {
		name       string
		sql        string
		route      planner.Route
		reason     planner.Reason
		unverified bool
		catalog    bool
	}{
		{
			name:   "constrained domain with a constant default",
			sql:    "ALTER TABLE t ADD COLUMN code positive_int DEFAULT 1",
			route:  planner.RouteCopyAndSwap,
			reason: planner.ReasonTypeRewrite,
		},
		{
			name:   "constrained domain without a default",
			sql:    "ALTER TABLE t ADD COLUMN strict positive_int",
			route:  planner.RouteCopyAndSwap,
			reason: planner.ReasonTypeRewrite,
		},
		{
			name:   "constrained domain with a stable default",
			sql:    "ALTER TABLE t ADD COLUMN scored positive_int DEFAULT next_score()",
			route:  planner.RouteCopyAndSwap,
			reason: planner.ReasonTypeRewrite,
		},
		{
			name:   "volatile default",
			sql:    "ALTER TABLE t ADD COLUMN id uuid DEFAULT gen_random_uuid()",
			route:  planner.RouteCopyAndSwap,
			reason: planner.ReasonVolatileDefault,
		},
		{
			name:   "stable default stored in the catalog",
			sql:    "ALTER TABLE t ADD COLUMN created timestamptz NOT NULL DEFAULT now()",
			route:  planner.RouteNative,
			reason: planner.ReasonFastDefault,
		},
		{
			name:    "constant default stored in the catalog",
			sql:     "ALTER TABLE t ADD COLUMN tier text DEFAULT 'free'",
			route:   planner.RouteNative,
			reason:  planner.ReasonFastDefault,
			catalog: true,
		},
		{
			name:   "column taking its domain's default",
			sql:    "ALTER TABLE t ADD COLUMN inherits defaulted_text",
			route:  planner.RouteNative,
			reason: planner.ReasonMetadataOnly,
		},
		{
			name:   "user function default stored in the catalog",
			sql:    "ALTER TABLE t ADD COLUMN once int DEFAULT one()",
			route:  planner.RouteNative,
			reason: planner.ReasonFastDefault,
		},
		{
			name:   "constant default through a user type",
			sql:    "ALTER TABLE t ADD COLUMN scope text DEFAULT 'a'::scoped::text",
			route:  planner.RouteNative,
			reason: planner.ReasonFastDefault,
		},
		{
			name:   "stored default with an inline check",
			sql:    "ALTER TABLE t ADD COLUMN checked timestamptz DEFAULT now() CHECK (checked > '2000-01-01')",
			route:  planner.RouteNative,
			reason: planner.ReasonSaferIdiom,
		},
		{
			name:    "plain column",
			sql:     "ALTER TABLE t ADD COLUMN age int",
			route:   planner.RouteNative,
			reason:  planner.ReasonMetadataOnly,
			catalog: true,
		},
		{
			name:       "expression default the server could not prove",
			sql:        "ALTER TABLE t ADD COLUMN high bigint DEFAULT max_id()",
			route:      planner.RouteCopyAndSwap,
			reason:     planner.ReasonVolatileDefault,
			unverified: true,
		},
		{
			name:       "constant default the server could not prove",
			sql:        "ALTER TABLE t ADD COLUMN capped bounded DEFAULT 5",
			route:      planner.RouteCopyAndSwap,
			reason:     planner.ReasonTypeRewrite,
			unverified: true,
		},
		{
			name:       "expression default with no proof run",
			sql:        "ALTER TABLE t ADD COLUMN other timestamptz DEFAULT now()",
			route:      planner.RouteCopyAndSwap,
			reason:     planner.ReasonVolatileDefault,
			unverified: true,
		},
		{
			name:   "constant default with no proof run",
			sql:    "ALTER TABLE t ADD COLUMN other int DEFAULT 0",
			route:  planner.RouteNative,
			reason: planner.ReasonFastDefault,
		},
		{
			name:   "NOT NULL the server checks by reading every row",
			sql:    "ALTER TABLE t ADD COLUMN filled int NOT NULL",
			route:  planner.RouteNative,
			reason: planner.ReasonMetadataOnly,
		},
		{
			name:   "NOT NULL with a default that stores no value",
			sql:    "ALTER TABLE t ADD COLUMN blank int NOT NULL DEFAULT nullif(1, 1)",
			route:  planner.RouteNative,
			reason: planner.ReasonFastDefault,
		},
		{
			name:   "generated stored whatever the proof says",
			sql:    "ALTER TABLE t ADD COLUMN age int GENERATED ALWAYS AS (1) STORED",
			route:  planner.RouteCopyAndSwap,
			reason: planner.ReasonGeneratedStored,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planner.Classify(tc.sql, proven)
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			assert.Equal(t, tc.route, plan.Decisions[0].Route)
			assert.Equal(t, tc.reason, plan.Decisions[0].Reason)
			assert.Equal(t, tc.unverified, plan.Decisions[0].Unverified)
			assert.Equal(t, tc.catalog, plan.Decisions[0].ProvenCatalogOnly,
				"only a column with no default or a literal one, that the server proved neither rewrites, scans the table, nor runs user code, is catalog-only")
		})
	}
}

// Dropping a column, a default, a NOT NULL, or a constraint, and renaming an
// index, change only the catalog: PostgreSQL never rewrites or reads the
// table and runs no user code for them, so each is marked catalog-only. Setting a default is not
// marked, since coercing it to a user-defined column type runs that type's
// input function under the lock. Changing a column's storage options or a
// table's options is metadata-only too but is not marked, and a cascading
// drop is refused before any mark.
func TestClassifyMarksCatalogOnlyKinds(t *testing.T) {
	cases := []struct {
		sql     string
		catalog bool
	}{
		{"ALTER TABLE t DROP COLUMN note", true},
		{"ALTER TABLE t ALTER COLUMN note SET DEFAULT 'x'", false},
		{"ALTER TABLE t ALTER COLUMN note DROP DEFAULT", true},
		{"ALTER TABLE t ALTER COLUMN note DROP NOT NULL", true},
		{"ALTER TABLE t DROP CONSTRAINT t_note_check", true},
		{"ALTER INDEX t_note_idx RENAME TO t_note_key", true},
		{"ALTER TABLE t ALTER COLUMN note SET STATISTICS 500", false},
		{"ALTER TABLE t SET (fillfactor = 90)", false},
		{"ALTER TABLE t DROP COLUMN note CASCADE", false},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			plan, err := planner.Classify(tc.sql, planner.Facts{})
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			assert.Equal(t, tc.catalog, plan.Decisions[0].ProvenCatalogOnly)
			assert.Equal(t, tc.catalog, planner.ProvenCatalogOnly(plan.Decisions))
		})
	}

	mixed, err := planner.Classify("ALTER TABLE t DROP COLUMN note, SET (fillfactor = 90)", planner.Facts{})
	require.NoError(t, err)
	assert.False(t, planner.ProvenCatalogOnly(mixed.Decisions),
		"one unmarked operation leaves the plan unproven")
	assert.False(t, planner.ProvenCatalogOnly(nil), "no decisions prove nothing")
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

// A VALIDATE CONSTRAINT scans under the strongest lock its ALTER TABLE
// takes, so it is the online idiom only when every operation in the
// statement takes SHARE UPDATE EXCLUSIVE alone: another validation, SET
// STATISTICS, a column option, or a storage parameter PostgreSQL changes
// under that lock. Beside the NOT VALID add it validates, SET STORAGE, or
// any other operation, it blocks, and no single safer statement exists.
func TestClassifyValidateIsOnlineOnlyBesideNonBlockingOperations(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		reason planner.Reason
	}{
		{"alone", "ALTER TABLE t VALIDATE CONSTRAINT c", planner.ReasonOnlineIdiom},
		{"beside another validate", "ALTER TABLE t VALIDATE CONSTRAINT c, VALIDATE CONSTRAINT d", planner.ReasonOnlineIdiom},
		{"beside the not valid add", "ALTER TABLE t ADD CONSTRAINT c CHECK (n > 0) NOT VALID, VALIDATE CONSTRAINT c", planner.ReasonSaferIdiom},
		{"beside a foreign key add", "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (pid) REFERENCES p (id) NOT VALID, VALIDATE CONSTRAINT fk", planner.ReasonSaferIdiom},
		{"beside an added column", "ALTER TABLE t ADD COLUMN x int, VALIDATE CONSTRAINT c", planner.ReasonSaferIdiom},
		{"beside a set statistics", "ALTER TABLE t ALTER COLUMN n SET STATISTICS 500, VALIDATE CONSTRAINT c", planner.ReasonOnlineIdiom},
		{"beside a column option", "ALTER TABLE t ALTER COLUMN n SET (n_distinct = 100), VALIDATE CONSTRAINT c", planner.ReasonOnlineIdiom},
		{"beside a fillfactor", "ALTER TABLE t SET (fillfactor = 70), VALIDATE CONSTRAINT c", planner.ReasonOnlineIdiom},
		{"beside a toast autovacuum reset", "ALTER TABLE t RESET (toast.autovacuum_enabled), VALIDATE CONSTRAINT c", planner.ReasonOnlineIdiom},
		{"beside a set storage", "ALTER TABLE t ALTER COLUMN n SET STORAGE PLAIN, VALIDATE CONSTRAINT c", planner.ReasonSaferIdiom},
		{"beside user_catalog_table", "ALTER TABLE t SET (user_catalog_table = true), VALIDATE CONSTRAINT c", planner.ReasonSaferIdiom},
		{"beside an unlisted parameter", "ALTER TABLE t SET (fillfactor = 70, made_up = 1), VALIDATE CONSTRAINT c", planner.ReasonSaferIdiom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planner.Classify(tc.sql, planner.Facts{})
			require.NoError(t, err)
			for _, d := range plan.Decisions {
				if d.Kind != statement.OpValidateConstraint {
					continue
				}
				assert.Equal(t, planner.RouteNative, d.Route)
				assert.Equal(t, tc.reason, d.Reason)
				assert.Empty(t, d.SaferSQL, "no single statement validates without the lock")
			}
		})
	}
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
		planner.ReasonDependentView,
		planner.ReasonDependentPolicy,
		planner.ReasonDependentTrigger,
		planner.ReasonDependentPublication,
		planner.ReasonDependentGeneratedColumn,
		planner.ReasonPartitionKey,
		planner.ReasonNotNullWithoutDefault,
		planner.ReasonColumnLimit,
		planner.ReasonCascadingDrop,
		planner.ReasonUnsupportedOperation,
	}, planner.Reasons())
}
