package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	liveNameIndex  = "CREATE INDEX events_name_idx ON events USING btree (name)"
	wantNameIndex  = "CREATE UNIQUE INDEX events_name_idx ON events USING btree (name)"
	builtNameIndex = "CREATE UNIQUE INDEX events_name_idx_new ON events USING btree (name)"
)

// desiredUniqueName is base() with events_name_idx made unique.
func desiredUniqueName() Model {
	m := base()
	m.Indexes = []Index{{Name: "events_name_idx", Def: wantNameIndex}}
	return m
}

// A changed index is replaced: its new definition is built as
// <name>_new while the old index keeps serving, then the old index is dropped
// and the new one renamed to its name. The drop names its replacement, so a
// consumer can tell it from a drop that removes the index's guarantee.
func TestDiffChangedIndexIsReplaced(t *testing.T) {
	changes, err := Diff("public", base(), desiredUniqueName())
	require.NoError(t, err)
	require.Equal(t, []string{
		`CREATE UNIQUE INDEX events_name_idx_new ON public.events USING btree (name)`,
		`DROP INDEX "public"."events_name_idx"`,
		`ALTER INDEX "public"."events_name_idx_new" RENAME TO "events_name_idx"`,
	}, sqls(changes))
	assert.Equal(t, []ChangeKind{ChangeCreateIndex, ChangeDropIndex, ChangeRenameIndex},
		[]ChangeKind{changes[0].Kind, changes[1].Kind, changes[2].Kind})

	drop := changes[1]
	assert.True(t, drop.Destructive)
	assert.Equal(t, "events_name_idx_new", drop.ReplacedBy)
	assert.Equal(t, &DroppedObject{Kind: DroppedIndex, Name: "events_name_idx", Definition: liveNameIndex}, drop.Drops)
	assert.False(t, changes[0].Destructive)
	assert.False(t, changes[2].Destructive)
	assert.Empty(t, changes[0].ReplacedBy)
}

// A run that stopped after building the replacement resumes at the drop: the
// valid build under the temporary name is kept, not dropped as an index the
// desired table does not name, and not built again.
func TestDiffChangedIndexResumesAfterTheBuild(t *testing.T) {
	live := base()
	live.Indexes = append(live.Indexes, Index{Name: "events_name_idx_new", Def: builtNameIndex})

	changes, err := Diff("public", live, desiredUniqueName())
	require.NoError(t, err)
	assert.Equal(t, []string{
		`DROP INDEX "public"."events_name_idx"`,
		`ALTER INDEX "public"."events_name_idx_new" RENAME TO "events_name_idx"`,
	}, sqls(changes))
	assert.Equal(t, "events_name_idx_new", changes[0].ReplacedBy)
}

// A run that stopped after dropping the old index has only the rename left.
// Planning a fresh build of the desired name, after dropping the build that
// already enforces it, would leave the table without the index again.
func TestDiffChangedIndexResumesAfterTheDrop(t *testing.T) {
	live := base()
	live.Indexes = []Index{{Name: "events_name_idx_new", Def: builtNameIndex}}

	changes, err := Diff("public", live, desiredUniqueName())
	require.NoError(t, err)
	assert.Equal(t, []string{
		`ALTER INDEX "public"."events_name_idx_new" RENAME TO "events_name_idx"`,
	}, sqls(changes))
}

// A failed build of the replacement leaves an invalid index under the
// temporary name. The next plan carries the same build, as it does for any
// unfinished build of the desired definition under the desired name, so the
// concurrent build path proves and clears the leftover; the old index stays
// until the build succeeds.
func TestDiffChangedIndexRebuildsAFailedReplacement(t *testing.T) {
	live := base()
	live.Indexes = append(live.Indexes, Index{Name: "events_name_idx_new", Def: builtNameIndex, Invalid: true})

	changes, err := Diff("public", live, desiredUniqueName())
	require.NoError(t, err)
	assert.Equal(t, []string{
		`CREATE UNIQUE INDEX events_name_idx_new ON public.events USING btree (name)`,
		`DROP INDEX "public"."events_name_idx"`,
		`ALTER INDEX "public"."events_name_idx_new" RENAME TO "events_name_idx"`,
	}, sqls(changes))
}

// The temporary name is refused when anything else holds it: an index of
// another definition, live or unfinished, or an object the desired or live
// table names. The replacement never drops or reuses an object it did not
// build.
func TestDiffChangedIndexRefusesATakenReplacementName(t *testing.T) {
	tests := []struct {
		name    string
		live    func(*Model)
		desired func(*Model)
		want    string
	}{
		{
			name: "a live index of another definition",
			live: func(m *Model) {
				m.Indexes = append(m.Indexes, Index{Name: "events_name_idx_new", Def: "CREATE INDEX events_name_idx_new ON events USING btree (id)"})
			},
			want: "which a live index on the table already names; rename that index",
		},
		{
			name: "an unfinished build of another definition",
			live: func(m *Model) {
				m.Indexes = append(m.Indexes, Index{Name: "events_name_idx_new", Def: "CREATE INDEX events_name_idx_new ON events USING btree (id)", Invalid: true})
			},
			want: `run DROP INDEX CONCURRENTLY "public"."events_name_idx_new" and plan again`,
		},
		{
			name: "a desired index",
			desired: func(m *Model) {
				m.Indexes = append(m.Indexes, Index{Name: "events_name_idx_new", Def: "CREATE INDEX events_name_idx_new ON events USING btree (id)"})
			},
			want: "which the desired table also names; rename that index",
		},
		{
			name: "a live constraint",
			live: func(m *Model) {
				m.Constraints = append(m.Constraints, Constraint{Name: "events_name_idx_new", Def: "CHECK ((id > 0))"})
			},
			want: "which the live table already names; rename that constraint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live, desired := base(), desiredUniqueName()
			if tt.live != nil {
				tt.live(&live)
			}
			if tt.desired != nil {
				tt.desired(&desired)
			}
			_, err := Diff("public", live, desired)
			require.ErrorIs(t, err, ErrUnsupportedChange)
			var u *UnsupportedChangeError
			require.ErrorAs(t, err, &u)
			assert.Equal(t, CauseReplacementIndexName, u.Cause)
			assert.Contains(t, err.Error(), `index "events_name_idx" is replaced by building its new definition as "events_name_idx_new"`)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// A table whose diff drops or retypes a column, and a partitioned parent,
// keep the drop and rebuild: the column change takes or rebuilds the index
// anyway, and a partitioned index is never built concurrently.
func TestDiffChangedIndexKeepsTheDropAndRebuild(t *testing.T) {
	t.Run("a retyped column", func(t *testing.T) {
		desired := desiredUniqueName()
		desired.Columns[1].Type = "text"
		changes, err := Diff("public", base(), desired)
		require.NoError(t, err)
		assert.Equal(t, `DROP INDEX "public"."events_name_idx"`, changes[0].SQL)
		assert.Empty(t, changes[0].ReplacedBy)
		assert.Equal(t, `CREATE UNIQUE INDEX events_name_idx ON public.events USING btree (name)`, changes[len(changes)-1].SQL)
	})
	t.Run("a partitioned parent", func(t *testing.T) {
		live, desired := base(), desiredUniqueName()
		live.PartitionKey, desired.PartitionKey = "RANGE (id)", "RANGE (id)"
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`DROP INDEX "public"."events_name_idx"`,
			`CREATE UNIQUE INDEX events_name_idx ON public.events USING btree (name)`,
		}, sqls(changes))
	})
}
