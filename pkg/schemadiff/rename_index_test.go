package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An index the file declares under a new name, with its definition
// otherwise unchanged, is renamed: the index keeps serving, and a unique
// one keeps rejecting duplicates, instead of being dropped and rebuilt.
func TestDiffRenamesAnIndexWhoseNameAloneChanges(t *testing.T) {
	for name, tc := range map[string]struct{ live, desired Index }{
		"a unique index": {
			live:    Index{Name: "events_code_uq", Def: "CREATE UNIQUE INDEX events_code_uq ON events USING btree (lower((name)::text))"},
			desired: Index{Name: "events_code_lower_uq", Def: "CREATE UNIQUE INDEX events_code_lower_uq ON events USING btree (lower((name)::text))"},
		},
		"a plain index": {
			live:    Index{Name: "events_name_idx", Def: "CREATE INDEX events_name_idx ON events USING btree (name)"},
			desired: Index{Name: "events_by_name", Def: "CREATE INDEX events_by_name ON events USING btree (name)"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			live, desired := base(), base()
			live.Indexes = []Index{tc.live}
			desired.Indexes = []Index{tc.desired}
			changes, err := Diff("public", live, desired)
			require.NoError(t, err)
			require.Len(t, changes, 1)
			assert.Equal(t, `ALTER INDEX "public".`+quoted(tc.live.Name)+` RENAME TO `+quoted(tc.desired.Name), changes[0].SQL)
			assert.Equal(t, ChangeRenameIndex, changes[0].Kind)
			assert.False(t, changes[0].Destructive)
		})
	}

	t.Run("an invalid index is rebuilt, not renamed", func(t *testing.T) {
		live, desired := base(), base()
		live.Indexes = []Index{{Name: "events_old_idx", Def: "CREATE INDEX events_old_idx ON events USING btree (name)", Invalid: true}}
		desired.Indexes = []Index{{Name: "events_by_name", Def: "CREATE INDEX events_by_name ON events USING btree (name)"}}
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{`CREATE INDEX events_by_name ON public.events USING btree (name)`}, sqls(changes),
			"an unfinished build is left to the build path")
	})

	t.Run("two live copies pair one way", func(t *testing.T) {
		live, desired := base(), base()
		live.Indexes = []Index{
			{Name: "events_b_idx", Def: "CREATE INDEX events_b_idx ON events USING btree (name)"},
			{Name: "events_a_idx", Def: "CREATE INDEX events_a_idx ON events USING btree (name)"},
		}
		desired.Indexes = []Index{{Name: "events_by_name", Def: "CREATE INDEX events_by_name ON events USING btree (name)"}}
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`DROP INDEX "public"."events_b_idx"`,
			`ALTER INDEX "public"."events_a_idx" RENAME TO "events_by_name"`,
		}, sqls(changes))
	})
}

// A uniqueness the file moves to another name or another kind of object is
// replaced before it is dropped: the plan builds the new unique index or adds
// the new unique constraint first, so duplicates are rejected throughout and
// a build that fails leaves the old uniqueness in place.
func TestDiffDropsAUniquenessOnlyAfterItsReplacement(t *testing.T) {
	t.Run("a unique constraint declared again as a unique index", func(t *testing.T) {
		live, desired := keyed(), keyed()
		live.Constraints[1] = Constraint{Name: "events_email_key", Def: "UNIQUE (email) INCLUDE (name)"}
		desired.Constraints = desired.Constraints[:1]
		desired.Indexes = append(desired.Indexes, Index{Name: "events_email_uq", Def: "CREATE UNIQUE INDEX events_email_uq ON events USING btree (email) INCLUDE (name)"})
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`CREATE UNIQUE INDEX events_email_uq ON public.events USING btree (email) INCLUDE (name)`,
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key"`,
		}, sqls(changes))
	})

	t.Run("a unique index renamed with another predicate", func(t *testing.T) {
		live, desired := base(), base()
		live.Indexes = []Index{{Name: "events_name_uq", Def: "CREATE UNIQUE INDEX events_name_uq ON events USING btree (name)"}}
		desired.Indexes = []Index{{Name: "events_live_name_uq", Def: "CREATE UNIQUE INDEX events_live_name_uq ON events USING btree (name) WHERE (id > 0)"}}
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`CREATE UNIQUE INDEX events_live_name_uq ON public.events USING btree (name) WHERE id > 0`,
			`DROP INDEX "public"."events_name_uq"`,
		}, sqls(changes))
	})

	t.Run("a check constraint declared again under another name", func(t *testing.T) {
		live, desired := base(), base()
		live.Constraints = append(live.Constraints, Constraint{Name: "events_id_check", Def: "CHECK ((id > 0))"})
		desired.Constraints = append(desired.Constraints, Constraint{Name: "events_id_positive", Def: "CHECK ((id > 0))"})
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_id_positive" CHECK ((id > 0))`,
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_id_check"`,
		}, sqls(changes))
	})

	t.Run("a unique index the file removes outright drops first", func(t *testing.T) {
		live, desired := base(), base()
		live.Indexes = []Index{{Name: "events_name_uq", Def: "CREATE UNIQUE INDEX events_name_uq ON events USING btree (name)"}}
		desired.Indexes = []Index{{Name: "events_name_idx", Def: "CREATE INDEX events_name_idx ON events USING btree (name)"}}
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`DROP INDEX "public"."events_name_uq"`,
			`CREATE INDEX events_name_idx ON public.events USING btree (name)`,
		}, sqls(changes), "no new uniqueness replaces it, so nothing waits")
	})

	t.Run("a plan that drops a column keeps its drops first", func(t *testing.T) {
		live, desired := keyed(), keyed()
		live.Columns = append(live.Columns, Column{Name: "legacy", Type: "text"})
		live.Indexes = []Index{{Name: "events_legacy_uq", Def: "CREATE UNIQUE INDEX events_legacy_uq ON events USING btree (legacy)"}}
		desired.Indexes = []Index{{Name: "events_email_uq", Def: "CREATE UNIQUE INDEX events_email_uq ON events USING btree (email)"}}
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, `DROP INDEX "public"."events_legacy_uq"`, changes[0].SQL)
	})
}

func quoted(name string) string { return `"` + name + `"` }
