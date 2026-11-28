package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const keptEmailIndexDef = "CREATE UNIQUE INDEX events_email_uidx ON events USING btree (email) INCLUDE (id)"

// withoutEmailKey is keyed() without its unique constraint on email.
func withoutEmailKey() Model {
	m := keyed()
	m.Constraints = m.Constraints[:1]
	return m
}

// dropOf returns the change that drops the named object, and its position.
func dropOf(t *testing.T, changes []Change, name string) (Change, int) {
	t.Helper()
	for i, ch := range changes {
		if ch.Drops != nil && ch.Drops.Name == name {
			return ch, i
		}
	}
	require.Failf(t, "no drop", "no change drops %s in %q", name, sqls(changes))
	return Change{}, -1
}

// A unique rule that moves to an object of another name, a constraint
// becoming a standalone index or an index becoming a constraint, is built
// before the old object is dropped. The drop names the object that keeps
// the rule, so a reviewer reads that no duplicate gets in.
func TestDiffNamesWhatKeepsADroppedUniqueRule(t *testing.T) {
	t.Run("a unique constraint becomes an index of another name", func(t *testing.T) {
		desired := withoutEmailKey()
		desired.Indexes = append(desired.Indexes, Index{Name: "events_email_uidx", Def: keptEmailIndexDef})
		changes, err := Diff("public", keyed(), desired)
		require.NoError(t, err)
		drop, at := dropOf(t, changes, "events_email_key")
		assert.True(t, drop.Destructive)
		assert.Equal(t, "events_email_uidx", drop.UniquenessKeptBy)
		assert.Equal(t, "CREATE UNIQUE INDEX events_email_uidx ON public.events USING btree (email) INCLUDE (id)", changes[at-1].SQL,
			"the index is built before the constraint is dropped")
	})

	t.Run("a unique index becomes a constraint of another name", func(t *testing.T) {
		live := withoutEmailKey()
		live.Indexes = append(live.Indexes, Index{Name: "events_email_uidx", Def: "CREATE UNIQUE INDEX events_email_uidx ON events USING btree (email)"})
		desired := keyed()
		desired.Constraints[1].IndexDef = "CREATE UNIQUE INDEX events_email_key ON events USING btree (email)"
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		drop, at := dropOf(t, changes, "events_email_uidx")
		assert.Equal(t, "events_email_key", drop.UniquenessKeptBy)
		assert.Equal(t, `ALTER TABLE "public"."events" ADD CONSTRAINT "events_email_key" UNIQUE (email)`, changes[at-1].SQL,
			"the constraint is in place before the index is dropped")
	})

	t.Run("a narrower rule that stands", func(t *testing.T) {
		live := keyed()
		live.Indexes = append(live.Indexes, Index{Name: "events_email_name_uidx", Def: "CREATE UNIQUE INDEX events_email_name_uidx ON events USING btree (email, name)"})
		changes, err := Diff("public", live, keyed())
		require.NoError(t, err)
		require.Len(t, changes, 1)
		assert.Equal(t, "events_email_key", changes[0].UniquenessKeptBy,
			"a key on email alone rejects every duplicate of email and name")
	})
}

// A drop is not credited to a rule that lets duplicates through, nor to one
// that is not in place yet when the drop runs.
func TestDiffNamesNothingForAWeakerOrLaterRule(t *testing.T) {
	t.Run("a wider rule", func(t *testing.T) {
		desired := withoutEmailKey()
		desired.Indexes = append(desired.Indexes, Index{Name: "events_email_name_uidx", Def: "CREATE UNIQUE INDEX events_email_name_uidx ON events USING btree (email, name)"})
		changes, err := Diff("public", keyed(), desired)
		require.NoError(t, err)
		drop, _ := dropOf(t, changes, "events_email_key")
		assert.Empty(t, drop.UniquenessKeptBy, "a rule over email and name lets two rows share an email")
	})

	t.Run("a partial rule", func(t *testing.T) {
		desired := withoutEmailKey()
		desired.Indexes = append(desired.Indexes, Index{Name: "events_email_uidx", Def: "CREATE UNIQUE INDEX events_email_uidx ON events USING btree (email) WHERE (name IS NOT NULL)"})
		changes, err := Diff("public", keyed(), desired)
		require.NoError(t, err)
		drop, _ := dropOf(t, changes, "events_email_key")
		assert.Empty(t, drop.UniquenessKeptBy, "rows outside the predicate can share an email")
	})

	t.Run("a rule built after a drop that goes first", func(t *testing.T) {
		live := keyed()
		live.Columns = append(live.Columns, Column{Name: "legacy", Type: "text", BuiltInType: true})
		desired := withoutEmailKey()
		desired.Indexes = append(desired.Indexes, Index{Name: "events_email_uidx", Def: keptEmailIndexDef})
		desired.Columns = append(desired.Columns, Column{Name: "score", Type: "integer", Default: "0", BuiltInType: true})
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		drop, at := dropOf(t, changes, "events_email_key")
		require.Equal(t, 0, at, "a plan that may rewrite the table drops first: %q", sqls(changes))
		assert.Empty(t, drop.UniquenessKeptBy, "the index is built after the constraint is gone")
	})
}
