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
