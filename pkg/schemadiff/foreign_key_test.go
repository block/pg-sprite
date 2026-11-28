package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A foreign-key constraint diffs under its own kinds, so a consumer can gate
// foreign-key changes apart from every other constraint.
func TestDiffClassifiesForeignKeyChanges(t *testing.T) {
	withFK := base()
	withFK.Constraints = append(withFK.Constraints, Constraint{
		Name:       "events_user_fk",
		Def:        "FOREIGN KEY (user_id) REFERENCES users(id)",
		ForeignKey: true,
	})

	added, err := Diff("app", base(), withFK)
	require.NoError(t, err)
	require.Len(t, added, 1)
	assert.Equal(t, ChangeAddForeignKey, added[0].Kind)
	assert.Equal(t, `ALTER TABLE app.events ADD CONSTRAINT events_user_fk FOREIGN KEY (user_id) REFERENCES app.users (id)`, added[0].SQL,
		"the referenced table is named in the target schema, so the statement binds it whatever search_path runs it")

	dropped, err := Diff("public", withFK, base())
	require.NoError(t, err)
	require.Len(t, dropped, 1)
	assert.Equal(t, ChangeDropForeignKey, dropped[0].Kind)
	assert.Equal(t, `ALTER TABLE "public"."events" DROP CONSTRAINT "events_user_fk"`, dropped[0].SQL)

	assert.True(t, ChangeAddForeignKey.IsForeignKey())
	assert.True(t, ChangeDropForeignKey.IsForeignKey())
	assert.False(t, ChangeAddConstraint.IsForeignKey())
	assert.False(t, ChangeDropConstraint.IsForeignKey())
}

// A foreign key the plan drops before the key that replaces it is added
// leaves writes checked against no key in between: every dropped key in a
// plan that adds a foreign key and also retypes a column, since such a plan
// keeps its drops first, and one changed under the same name on a
// partitioned parent, which cannot take a NOT VALID key. A key changed under
// the same name elsewhere is replaced in one statement, a key moved to a new
// name waits for its replacement and is dropped after it, a plan that only
// drops columns drops them after every replacement, and a plan that adds no
// foreign key replaces nothing.
func TestForeignKeysDroppedBeforeTheirReplacement(t *testing.T) {
	fk := func(name, def string) Constraint {
		return Constraint{Name: name, Def: def, ForeignKey: true}
	}
	withKeys := func(m Model, keys ...Constraint) Model {
		m.Constraints = append(m.Constraints, keys...)
		return m
	}
	live := withKeys(base(), fk("events_user_fk", "FOREIGN KEY (user_id) REFERENCES users(id)"))
	cascade := "FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE"
	withoutName := func(m Model) Model {
		m.Columns = m.Columns[:1]
		return m
	}
	retypedName := func(m Model) Model {
		m.Columns[1].Type = "text"
		return m
	}

	for _, tt := range []struct {
		name    string
		desired Model
		want    map[string]bool
	}{
		{"changed under the same name", withKeys(base(), fk("events_user_fk", cascade)), map[string]bool{}},
		{"moved to a new name", withKeys(base(), fk("events_user_cascade_fk", cascade)), map[string]bool{}},
		{"moved to a new name beside a column drop", withKeys(withoutName(base()), fk("events_user_cascade_fk", cascade)), map[string]bool{}},
		{"moved to a new name beside a column retype", withKeys(retypedName(base()), fk("events_user_cascade_fk", cascade)), map[string]bool{"events_user_fk": true}},
		{"dropped beside a column drop, with nothing added", withoutName(base()), map[string]bool{}},
		{"dropped", base(), map[string]bool{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ForeignKeysDroppedBeforeTheirReplacement(live, tt.desired))
		})
	}

	// The keys a column change drops first are their own set, apart from a
	// partitioned parent's key changed under the same name.
	assert.Equal(t, map[string]bool{"events_user_fk": true},
		ForeignKeysDroppedForColumnChange(live, withKeys(retypedName(base()), fk("events_user_cascade_fk", cascade))),
		"a retype that adds a key drops the old key first, whatever the new key's name")
	assert.Equal(t, map[string]bool{},
		ForeignKeysDroppedForColumnChange(live, withKeys(withoutName(base()), fk("events_user_cascade_fk", cascade))),
		"a column drop beside no rewriting add keeps its drops last")

	parent := live
	parent.PartitionKey = "RANGE (id)"
	assert.Equal(t, map[string]bool{"events_user_fk": true},
		ForeignKeysDroppedBeforeTheirReplacement(parent, withKeys(base(), fk("events_user_fk", cascade))),
		"a partitioned parent drops a key changed under the same name before adding it again")
	assert.Equal(t, map[string]bool{},
		ForeignKeysDroppedForColumnChange(parent, withKeys(base(), fk("events_user_fk", cascade))),
		"a partitioned parent's key changed under the same name is not a column-first drop")

	moved, err := Diff("app", live, withKeys(base(), fk("events_user_cascade_fk", cascade)))
	require.NoError(t, err)
	require.Len(t, moved, 2)
	assert.Equal(t, ChangeAddForeignKey, moved[0].Kind, "the new key is added first")
	assert.Equal(t, ChangeDropForeignKey, moved[1].Kind, "the old key is dropped once the new one is in place")

	// A move whose validation failed left the new key NOT VALID beside the old
	// one. The next plan validates the new key and drops the old one.
	pending := fk("events_user_cascade_fk", cascade+notValidSuffix)
	pending.NotValid = true
	stalled := withKeys(base(), live.Constraints[len(live.Constraints)-1], pending)
	assert.Equal(t, map[string]bool{}, ForeignKeysDroppedBeforeTheirReplacement(stalled, withKeys(base(), fk("events_user_cascade_fk", cascade))))
	resumed, err := Diff("app", stalled, withKeys(base(), fk("events_user_cascade_fk", cascade)))
	require.NoError(t, err)
	require.Len(t, resumed, 2)
	assert.Equal(t, ChangeValidateForeignKey, resumed[0].Kind, "the new key is validated first")
	assert.Equal(t, ChangeDropForeignKey, resumed[1].Kind)
	assert.Equal(t, "events_user_fk", resumed[1].Drops.Name)
}
