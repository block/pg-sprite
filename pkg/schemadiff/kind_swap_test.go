package schemadiff

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

const (
	swapConstraintDef = "UNIQUE (email)"
	swapIndexDef      = "CREATE UNIQUE INDEX events_email_key ON events USING btree (email) INCLUDE (name, id)"
	swapIndexTempDef  = "CREATE UNIQUE INDEX events_email_key_new ON events USING btree (email) INCLUDE (name, id)"
	swapKeyDef        = "UNIQUE (email) INCLUDE (name, id)"
	swapKeyIndexDef   = "CREATE UNIQUE INDEX events_email_key ON events USING btree (email) INCLUDE (name, id)"
	swapKeyTempDef    = "CREATE UNIQUE INDEX events_email_key_new ON events USING btree (email) INCLUDE (name, id)"
)

// swapConstraintTable is keyed() with events_email_key as a unique
// constraint, the live side of a constraint-to-index swap.
func swapConstraintTable() Model {
	return keyed()
}

// swapIndexTable is keyed() with events_email_key as a standalone unique
// index of def instead of a constraint.
func swapIndexTable(def string) Model {
	m := keyed()
	m.Constraints = m.Constraints[:1]
	m.Indexes = append(m.Indexes, Index{Name: "events_email_key", Def: def})
	return m
}

// swapKeyTable is keyed() with events_email_key as the unique constraint a
// file declares, with the index it owns.
func swapKeyTable() Model {
	m := keyed()
	m.Constraints[1] = Constraint{Name: "events_email_key", Def: swapKeyDef, IndexDef: swapKeyIndexDef}
	return m
}

// A file that turns a unique constraint into a standalone unique index of
// the same name keeps the constraint enforcing its rule while the index
// builds under a temporary name. Only then is the constraint dropped, while
// the new index is still in place, and the new index renamed to its name.
// Dropping first would let duplicates in for as long as the build takes, and
// a failed build would leave no unique rule at all.
func TestDiffSameNameKindSwapBuildsFirst(t *testing.T) {
	t.Run("a unique constraint becomes a unique index", func(t *testing.T) {
		for name, def := range map[string]string{
			"of another definition": swapIndexDef,
			"of the same columns":   "CREATE UNIQUE INDEX events_email_key ON events USING btree (email)",
		} {
			changes, err := Diff("public", swapConstraintTable(), swapIndexTable(def))
			require.NoError(t, err, name)
			temp, err := statement.RenameIndex(def, "events_email_key_new")
			require.NoError(t, err)
			create, err := statement.Qualify(temp, "public")
			require.NoError(t, err)
			require.Equal(t, []string{
				create,
				`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key"`,
				`ALTER INDEX "public"."events_email_key_new" RENAME TO "events_email_key"`,
			}, sqls(changes), name)
			assert.Equal(t, ChangeCreateIndex, changes[0].Kind, name)

			drop := changes[1]
			assert.Equal(t, ChangeDropConstraint, drop.Kind, name)
			assert.True(t, drop.Destructive, name)
			assert.Equal(t, &DroppedObject{Kind: DroppedConstraint, Name: "events_email_key", Definition: swapConstraintDef}, drop.Drops, name)
			assert.Equal(t, "events_email_key_new", drop.ReplacedBy, name)
			assert.Equal(t, &IndexReplacement{Index: "events_email_key_new", Def: temp}, drop.Replacement,
				"%s: the constraint is dropped only while the new index is in place", name)

			assert.Equal(t, &IndexRename{Index: "events_email_key_new", Def: temp}, changes[2].Renames, name)
		}
	})

	t.Run("a unique index becomes a unique constraint", func(t *testing.T) {
		changes, err := Diff("public", swapIndexTable("CREATE UNIQUE INDEX events_email_key ON events USING btree (email)"), swapKeyTable())
		require.NoError(t, err)
		require.Equal(t, []string{
			"CREATE UNIQUE INDEX events_email_key_new ON public.events USING btree (email) INCLUDE (name, id)",
			`DROP INDEX "public"."events_email_key"`,
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key_new"`,
		}, sqls(changes))
		drop := changes[1]
		assert.Equal(t, ChangeDropIndex, drop.Kind)
		assert.Equal(t, &DroppedObject{Kind: DroppedIndex, Name: "events_email_key", Definition: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email)"}, drop.Drops)
		assert.Equal(t, &IndexReplacement{Index: "events_email_key_new", Def: swapKeyTempDef}, drop.Replacement,
			"the old index is dropped only while the new one is in place")
		add := changes[2]
		assert.Equal(t, ChangeAddConstraint, add.Kind)
		assert.False(t, add.Destructive)
		assert.Equal(t, &IndexAdoption{Index: "events_email_key_new", Def: swapKeyTempDef}, add.Adopts)
	})

	t.Run("a unique index of the constraint's definition is adopted", func(t *testing.T) {
		changes, err := Diff("public", swapIndexTable(swapKeyIndexDef), swapKeyTable())
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key"`,
		}, sqls(changes))
	})
}

// A swap an earlier run left part-way is picked up where it stopped: a
// valid build under the temporary name is not built again, and once the old
// object is gone only the last step is left.
func TestDiffResumesASameNameKindSwap(t *testing.T) {
	withTemp := func(m Model, def string) Model {
		m.Indexes = append(m.Indexes, Index{Name: "events_email_key_new", Def: def})
		return m
	}

	t.Run("constraint to index, after the build", func(t *testing.T) {
		changes, err := Diff("public", withTemp(swapConstraintTable(), swapIndexTempDef), swapIndexTable(swapIndexDef))
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key"`,
			`ALTER INDEX "public"."events_email_key_new" RENAME TO "events_email_key"`,
		}, sqls(changes))
	})

	t.Run("constraint to index, after the drop", func(t *testing.T) {
		live := keyed()
		live.Constraints = live.Constraints[:1]
		changes, err := Diff("public", withTemp(live, swapIndexTempDef), swapIndexTable(swapIndexDef))
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER INDEX "public"."events_email_key_new" RENAME TO "events_email_key"`,
		}, sqls(changes))
	})

	t.Run("index to constraint, after the build", func(t *testing.T) {
		live := withTemp(swapIndexTable("CREATE UNIQUE INDEX events_email_key ON events USING btree (email)"), swapKeyTempDef)
		changes, err := Diff("public", live, swapKeyTable())
		require.NoError(t, err)
		assert.Equal(t, []string{
			`DROP INDEX "public"."events_email_key"`,
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key_new"`,
		}, sqls(changes))
	})

	t.Run("index to constraint, after the drop", func(t *testing.T) {
		live := keyed()
		live.Constraints = live.Constraints[:1]
		changes, err := Diff("public", withTemp(live, swapKeyTempDef), swapKeyTable())
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key_new"`,
		}, sqls(changes))
		assert.Equal(t, &IndexAdoption{Index: "events_email_key_new", Def: swapKeyTempDef}, changes[0].Adopts)
	})

	t.Run("a build of another definition is refused", func(t *testing.T) {
		live := withTemp(swapIndexTable("CREATE UNIQUE INDEX events_email_key ON events USING btree (email)"),
			"CREATE UNIQUE INDEX events_email_key_new ON events USING btree (name)")
		_, err := Diff("public", live, swapKeyTable())
		var refusal *UnsupportedChangeError
		require.True(t, errors.As(err, &refusal), "%v", err)
		assert.Equal(t, CauseReplacementIndexName, refusal.Cause)

		_, err = Diff("public", withTemp(swapConstraintTable(), "CREATE UNIQUE INDEX events_email_key_new ON events USING btree (name)"), swapIndexTable(swapIndexDef))
		require.True(t, errors.As(err, &refusal), "%v", err)
		assert.Equal(t, CauseReplacementIndexName, refusal.Cause)
	})
}

// A swap whose old object a foreign key references is refused before
// anything is built: PostgreSQL does not drop an index or key a foreign key
// depends on.
func TestDiffRefusesASameNameKindSwapAForeignKeyReferences(t *testing.T) {
	ref := []IncomingForeignKey{{Schema: "public", Table: "orders", Constraint: "orders_email_fkey"}}

	live := swapConstraintTable()
	live.Constraints[1].ReferencedBy = ref
	_, err := Diff("public", live, swapIndexTable(swapIndexDef))
	var refusal *UnsupportedChangeError
	require.True(t, errors.As(err, &refusal), "%v", err)
	assert.Equal(t, CauseReferencedIndex, refusal.Cause)

	live = swapIndexTable("CREATE UNIQUE INDEX events_email_key ON events USING btree (email)")
	live.Indexes[len(live.Indexes)-1].ReferencedBy = ref
	_, err = Diff("public", live, swapKeyTable())
	require.True(t, errors.As(err, &refusal), "%v", err)
	assert.Equal(t, CauseReferencedIndex, refusal.Cause)
}

// A table whose diff drops a column keeps the drop first: a dropped column
// takes the indexes and constraints that read it with it.
func TestDiffSameNameKindSwapDropsFirstWithAColumnDrop(t *testing.T) {
	live := swapConstraintTable()
	live.Columns = append(live.Columns, Column{Name: "legacy", Type: "text"})
	changes, err := Diff("public", live, swapIndexTable(swapIndexDef))
	require.NoError(t, err)
	assert.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key"`,
		`ALTER TABLE "public"."events" DROP COLUMN "legacy"`,
		"CREATE UNIQUE INDEX events_email_key ON public.events USING btree (email) INCLUDE (name, id)",
	}, sqls(changes))
}

// A unique index whose definition the file keeps under another name, while
// a unique constraint takes its old name, is not swapped for the constraint:
// dropping it would leave its columns without a unique rule while the index
// of the new name builds. The index must be renamed before the constraint
// can take the name, so the plan is refused with renamed-index-name.
func TestDiffRefusesAConstraintOnTheNameOfARenamedIndex(t *testing.T) {
	live := swapIndexTable("CREATE UNIQUE INDEX events_email_key ON events USING btree (email)")
	desired := keyed()
	desired.Constraints[1] = Constraint{
		Name:     "events_email_key",
		Def:      "UNIQUE (name)",
		IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (name)",
	}
	desired.Indexes = append(desired.Indexes, Index{Name: "events_email_uniq", Def: "CREATE UNIQUE INDEX events_email_uniq ON events USING btree (email)"})

	_, err := Diff("public", live, desired)
	var refusal *UnsupportedChangeError
	require.True(t, errors.As(err, &refusal), "%v", err)
	assert.Equal(t, CauseRenamedIndexName, refusal.Cause)
	assert.Contains(t, err.Error(), `constraint "events_email_key" takes the name of index "events_email_key", which the desired table renames to "events_email_uniq"`)
}

// A unique index swapped for a constraint of its name is refused when
// another unique index the file builds takes over its key, whether that
// index replaces a live one of the same name or is new and narrower: the
// swap would drop the live index before the other is built, leaving the key
// without a unique rule while it builds. An index of that key that already
// stands is no reason to refuse.
func TestDiffRefusesAKindSwapWhoseKeyMovesToABuild(t *testing.T) {
	foo := "CREATE UNIQUE INDEX events_email_key ON events USING btree (email)"
	withIndex := func(m Model, name, def string) Model {
		m.Indexes = append(append([]Index(nil), m.Indexes...), Index{Name: name, Def: def})
		return m
	}
	desiredWith := func(def string) Model {
		m := keyed()
		m.Constraints[1] = Constraint{
			Name:     "events_email_key",
			Def:      "UNIQUE (name)",
			IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (name)",
		}
		return withIndex(m, "events_email_uniq", def)
	}

	for name, tc := range map[string]struct {
		live    Model
		desired string
	}{
		"onto a live index it rebuilds unique": {
			live:    withIndex(swapIndexTable(foo), "events_email_uniq", "CREATE INDEX events_email_uniq ON events USING btree (email)"),
			desired: "CREATE UNIQUE INDEX events_email_uniq ON events USING btree (email)",
		},
		"onto a new, narrower index": {
			live:    swapIndexTable(foo),
			desired: "CREATE UNIQUE INDEX events_email_uniq ON events USING btree (email) WHERE (name IS NOT NULL)",
		},
	} {
		_, err := Diff("public", tc.live, desiredWith(tc.desired))
		var refusal *UnsupportedChangeError
		require.True(t, errors.As(err, &refusal), "%s: %v", name, err)
		assert.Equal(t, CauseMovedUniqueRule, refusal.Cause, name)
		assert.Contains(t, err.Error(), `constraint "events_email_key" takes the name of unique index "events_email_key", whose key the desired table's index "events_email_uniq" also makes unique`, name)
	}

	t.Run("an index of the key that already stands", func(t *testing.T) {
		standing := "CREATE UNIQUE INDEX events_email_uniq ON events USING btree (email)"
		changes, err := Diff("public", withIndex(swapIndexTable(foo), "events_email_uniq", standing), desiredWith(standing))
		require.NoError(t, err)
		assert.Equal(t, []string{
			"CREATE UNIQUE INDEX events_email_key_new ON public.events USING btree (name)",
			`DROP INDEX "public"."events_email_key"`,
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key_new"`,
		}, sqls(changes))
	})
}
