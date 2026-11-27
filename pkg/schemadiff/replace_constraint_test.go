package schemadiff

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyed returns base() with its primary key marked as one and a unique
// constraint on name, the shape introspection reports for a live table.
func keyed() Model {
	m := base()
	m.Columns = append(m.Columns, Column{Name: "email", Type: "text"})
	m.Constraints = []Constraint{
		{Name: "events_pkey", Def: "PRIMARY KEY (id)", Primary: true},
		{Name: "events_email_key", Def: "UNIQUE (email)"},
	}
	return m
}

// A unique constraint whose columns change keeps enforcing the old rule
// while its new index builds under a temporary name. One ALTER TABLE then
// drops the old constraint and attaches the new index, so the table never
// goes without either rule, and a failed build leaves the old one in place.
func TestDiffReplacesAUniqueConstraintInPlace(t *testing.T) {
	desired := keyed()
	desired.Constraints[1] = Constraint{
		Name: "events_email_key", Def: "UNIQUE (email, name)",
		IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email, name)",
	}
	changes, err := Diff("public", keyed(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`CREATE UNIQUE INDEX events_email_key_new ON public.events USING btree (email, name)`,
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key", ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key_new"`,
	}, sqls(changes))
	assert.Equal(t, ChangeCreateIndex, changes[0].Kind)
	assert.False(t, changes[0].Destructive, "building the new index removes nothing")
	// The planner builds every created index CONCURRENTLY, this one included.

	swap := changes[1]
	assert.Equal(t, ChangeAddConstraint, swap.Kind)
	assert.True(t, swap.Destructive)
	assert.Equal(t, &IndexAdoption{
		Index: "events_email_key_new",
		Def:   "CREATE UNIQUE INDEX events_email_key_new ON events USING btree (email, name)",
	}, swap.Adopts, "execution checks the built index before it attaches it")
	assert.Equal(t, &DroppedObject{Kind: DroppedConstraint, Name: "events_email_key", Definition: "UNIQUE (email)"}, swap.Drops)
}

func TestDiffReplacesAPrimaryKeyInPlace(t *testing.T) {
	desired := keyed()
	desired.Constraints[0] = Constraint{
		Name: "events_pkey", Def: "PRIMARY KEY (id, name)", Primary: true,
		IndexDef: "CREATE UNIQUE INDEX events_pkey ON events USING btree (id, name)",
	}
	changes, err := Diff("public", keyed(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`CREATE UNIQUE INDEX events_pkey_new ON public.events USING btree (id, name)`,
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_pkey", ADD CONSTRAINT "events_pkey" PRIMARY KEY USING INDEX "events_pkey_new"`,
	}, sqls(changes))
	assert.Equal(t, &DroppedObject{Kind: DroppedConstraint, Name: "events_pkey", Definition: "PRIMARY KEY (id)"}, changes[1].Drops)
}

func TestDiffReplacesADeferrableKeyInPlace(t *testing.T) {
	desired := keyed()
	desired.Constraints[1] = Constraint{
		Name: "events_email_key", Def: "UNIQUE (email) DEFERRABLE INITIALLY DEFERRED",
		IndexDef:   "CREATE UNIQUE INDEX events_email_key ON events USING btree (email)",
		Deferrable: true, InitiallyDeferred: true,
	}
	changes, err := Diff("public", keyed(), desired)
	require.NoError(t, err)
	require.Len(t, changes, 2)
	assert.Equal(t,
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key", ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key_new" DEFERRABLE INITIALLY DEFERRED`,
		changes[1].SQL)
}

// A CHECK whose definition changes is swapped in one statement that adds the
// new rule NOT VALID, so new writes are checked by one rule or the other at
// every moment. The existing rows are scanned afterwards by a VALIDATE that
// blocks no writes.
func TestDiffReplacesACheckInPlace(t *testing.T) {
	live := base()
	live.Constraints = append(live.Constraints, Constraint{Name: "events_name_check", Def: "CHECK ((length((name)::text) > 0))"})
	desired := base()
	desired.Constraints = append(desired.Constraints, Constraint{Name: "events_name_check", Def: "CHECK ((length((name)::text) > 2))"})
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_name_check", ADD CONSTRAINT "events_name_check" CHECK ((length((name)::text) > 2)) NOT VALID`,
		`ALTER TABLE "public"."events" VALIDATE CONSTRAINT "events_name_check"`,
	}, sqls(changes))
	assert.True(t, changes[0].Destructive)
	assert.Equal(t, &DroppedObject{Kind: DroppedConstraint, Name: "events_name_check", Definition: "CHECK ((length((name)::text) > 0))"}, changes[0].Drops)
	assert.False(t, changes[1].Destructive)
	require.NotNil(t, changes[1].Validates, "the VALIDATE names the rule it settles")
}

// A FOREIGN KEY whose definition changes keeps its drop and re-add, so the
// drop is planned as a foreign-key drop and the plan's foreign-key drop
// policy decides it, as it does for a key the file no longer declares.
func TestDiffKeepsAChangedForeignKeysDropAndAdd(t *testing.T) {
	live := base()
	live.Constraints = append(live.Constraints, Constraint{
		Name: "events_owner_fkey", Def: "FOREIGN KEY (id) REFERENCES owners(id)", ForeignKey: true,
	})
	desired := base()
	desired.Constraints = append(desired.Constraints, Constraint{
		Name: "events_owner_fkey", Def: "FOREIGN KEY (id) REFERENCES owners(id) ON DELETE CASCADE", ForeignKey: true,
	})
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_owner_fkey"`,
		`ALTER TABLE public.events ADD CONSTRAINT events_owner_fkey FOREIGN KEY (id) REFERENCES public.owners (id) ON DELETE CASCADE`,
	}, sqls(changes))
	assert.Equal(t, ChangeDropForeignKey, changes[0].Kind, "the drop is the one the foreign-key drop policy reads")
}

// A primary key narrowed to fewer columns, on a table whose file also lets a
// column it drops be NULL, is replaced in place like any other key. The old
// key owns that column's NOT NULL until the swap drops it, so the DROP NOT
// NULL comes after the swap: the new key is built while the old one keeps
// rejecting duplicates, and a build that fails leaves the old key and the
// NOT NULL in place.
func TestDiffDropsANarrowedPrimaryKeysNotNullAfterTheSwap(t *testing.T) {
	live := keyed()
	live.Constraints[0] = Constraint{Name: "events_pkey", Def: "PRIMARY KEY (id, name)", Primary: true}
	desired := keyed()
	desired.Constraints[0] = Constraint{
		Name: "events_pkey", Def: "PRIMARY KEY (id)", Primary: true,
		IndexDef: "CREATE UNIQUE INDEX events_pkey ON events USING btree (id)",
	}
	for i := range desired.Columns {
		if desired.Columns[i].Name == "name" {
			desired.Columns[i].NotNull = false
		}
	}
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	got := sqls(changes)
	require.Len(t, got, 3, "%v", got)
	assert.Contains(t, got[0], "CREATE UNIQUE INDEX events_pkey_new")
	assert.Contains(t, got[1], `DROP CONSTRAINT "events_pkey"`)
	assert.Contains(t, got[1], "USING INDEX")
	assert.Equal(t, `ALTER TABLE "public"."events" ALTER COLUMN "name" DROP NOT NULL`, got[2],
		"the NOT NULL the old key owned is dropped once the key is swapped")
}

// A replaced key's new index is built under the constraint's name with a
// suffix. A desired or live object that already has that name is refused
// with a typed cause before any statement runs, rather than failing the
// build half-way through an apply.
func TestDiffRefusesAReplacementIndexNameInUse(t *testing.T) {
	changed := Constraint{
		Name: "events_email_key", Def: "UNIQUE (email, name)",
		IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email, name)",
	}
	cases := map[string]func(live, desired *Model){
		"a desired index": func(_, desired *Model) {
			desired.Indexes = append(desired.Indexes, Index{
				Name: "events_email_key_new", Def: "CREATE INDEX events_email_key_new ON events USING btree (email)",
			})
		},
		"a desired constraint": func(_, desired *Model) {
			desired.Constraints = append(desired.Constraints, Constraint{Name: "events_email_key_new", Def: "CHECK ((id > 0))"})
		},
		"a live constraint": func(live, desired *Model) {
			con := Constraint{Name: "events_email_key_new", Def: "CHECK ((id > 0))"}
			live.Constraints = append(live.Constraints, con)
			desired.Constraints = append(desired.Constraints, con)
		},
		"a live index the desired table does not declare": func(live, _ *Model) {
			live.Indexes = append(live.Indexes, Index{
				Name: "events_email_key_new", Def: "CREATE INDEX events_email_key_new ON public.events USING btree (email)",
			})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			live, desired := keyed(), keyed()
			desired.Constraints[1] = changed
			mutate(&live, &desired)
			_, err := Diff("public", live, desired)
			require.ErrorIs(t, err, ErrUnsupportedChange)
			var refusal *UnsupportedChangeError
			require.True(t, errors.As(err, &refusal))
			assert.Equal(t, CauseReplacementIndexName, refusal.Cause)
			assert.Contains(t, err.Error(), `"events_email_key_new"`)
		})
	}
}

// A diff that drops or retypes a column keeps the drop and re-add: the
// column change itself removes or re-checks the constraints that read the
// column, so a constraint swapped in before it would be checked twice.
func TestDiffKeepsDropAndReaddWhenAColumnChanges(t *testing.T) {
	t.Run("a dropped column", func(t *testing.T) {
		live := keyed()
		live.Columns = append(live.Columns, Column{Name: "legacy", Type: "integer"})
		desired := keyed()
		desired.Constraints[1] = Constraint{
			Name: "events_email_key", Def: "UNIQUE (email, name)",
			IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email, name)",
		}
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		for _, c := range changes {
			assert.NotContains(t, c.SQL, "events_email_key_new")
		}
		assert.Contains(t, sqls(changes), `ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key"`)
	})

	t.Run("a retyped column", func(t *testing.T) {
		live := base()
		live.Constraints = append(live.Constraints, Constraint{Name: "events_name_check", Def: "CHECK ((length((name)::text) > 0))"})
		desired := base()
		desired.Columns[1].Type = "text"
		desired.Constraints = append(desired.Constraints, Constraint{Name: "events_name_check", Def: "CHECK ((length(name) > 2))"})
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Contains(t, sqls(changes), `ALTER TABLE "public"."events" DROP CONSTRAINT "events_name_check"`)
	})
}

// A constraint that changes kind (a unique key becoming a check, a check
// becoming a foreign key) is not one rule replacing another and keeps the
// drop and re-add.
func TestDiffKeepsDropAndReaddAcrossConstraintKinds(t *testing.T) {
	live := base()
	live.Constraints = append(live.Constraints, Constraint{Name: "events_name_rule", Def: "CHECK ((id > 0))"})
	desired := base()
	desired.Constraints = append(desired.Constraints, Constraint{
		Name: "events_name_rule", Def: "FOREIGN KEY (id) REFERENCES owners(id)", ForeignKey: true,
	})
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Contains(t, sqls(changes), `ALTER TABLE "public"."events" DROP CONSTRAINT "events_name_rule"`)
}

func TestReplacementIndexName(t *testing.T) {
	assert.Equal(t, "events_email_key_new", replacementIndexName("events_email_key"))

	long := strings.Repeat("a", 60)
	name := replacementIndexName(long)
	assert.LessOrEqual(t, len(name), maxIdentifierBytes)
	assert.True(t, strings.HasSuffix(name, replacementIndexSuffix))
	assert.NotEqual(t, name, replacementIndexName(strings.Repeat("a", 59)+"b"),
		"two long names that share a prefix never cut to the same temporary name")

	multibyte := strings.Repeat("é", 31)
	cut := replacementIndexName(multibyte)
	assert.LessOrEqual(t, len(cut), maxIdentifierBytes)
	assert.True(t, utf8.ValidString(cut), "the cut never splits a character")
}

// An earlier attempt whose new index failed to build leaves that build
// invalid under the temporary name. The next plan refuses rather than build
// into the name, and names the statement that clears the leftover.
func TestDiffRefusesAnEarlierAttemptsUnfinishedReplacementIndex(t *testing.T) {
	live, desired := keyed(), keyed()
	desired.Constraints[1] = Constraint{
		Name: "events_email_key", Def: "UNIQUE (email, name)",
		IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email, name)",
	}
	live.Indexes = append(live.Indexes, Index{
		Name:    "events_email_key_new",
		Def:     "CREATE UNIQUE INDEX events_email_key_new ON public.events USING btree (email, name)",
		Invalid: true,
	})
	_, err := Diff("public", live, desired)
	var refusal *UnsupportedChangeError
	require.True(t, errors.As(err, &refusal), "%v", err)
	assert.Equal(t, CauseReplacementIndexName, refusal.Cause)
	assert.Contains(t, err.Error(), `DROP INDEX CONCURRENTLY "public"."events_email_key_new"`)
	assert.Contains(t, err.Error(), "pg_stat_progress_create_index", "the reader checks no build is running before the drop")
}

// A unique constraint whose new index an earlier run built and left valid
// under its temporary name is swapped in without a second build, and the
// built index is not dropped as one the desired table does not declare.
func TestDiffResumesAUniqueConstraintReplacementAfterItsBuild(t *testing.T) {
	live, desired := keyed(), keyed()
	desired.Constraints[1] = Constraint{
		Name: "events_email_key", Def: "UNIQUE (email, name)",
		IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email, name)",
	}
	live.Indexes = append(live.Indexes, Index{
		Name: "events_email_key_new",
		Def:  "CREATE UNIQUE INDEX events_email_key_new ON events USING btree (email, name)",
	})
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key", ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key_new"`,
	}, sqls(changes))
	assert.Equal(t, "events_email_key_new", changes[0].Adopts.Index)

	t.Run("a build of another definition is refused", func(t *testing.T) {
		other := keyed()
		other.Indexes = append(other.Indexes, Index{
			Name: "events_email_key_new",
			Def:  "CREATE UNIQUE INDEX events_email_key_new ON events USING btree (email)",
		})
		_, err := Diff("public", other, desired)
		var refusal *UnsupportedChangeError
		require.True(t, errors.As(err, &refusal), "%v", err)
		assert.Equal(t, CauseReplacementIndexName, refusal.Cause)
	})
}

// A key a foreign key references cannot be dropped, so replacing it is
// refused before anything is built, naming the foreign key.
func TestDiffRefusesReplacingAKeyAForeignKeyReferences(t *testing.T) {
	live, desired := keyed(), keyed()
	live.Constraints[1].ReferencedBy = []IncomingForeignKey{{Schema: "public", Table: "orders", Constraint: "orders_email_fkey"}}
	desired.Constraints[1] = Constraint{
		Name: "events_email_key", Def: "UNIQUE (email, name)",
		IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email, name)",
	}
	_, err := Diff("public", live, desired)
	var refusal *UnsupportedChangeError
	require.True(t, errors.As(err, &refusal), "%v", err)
	assert.Equal(t, CauseReferencedIndex, refusal.Cause)
	assert.Contains(t, err.Error(), `constraint "events_email_key" is referenced by foreign key "orders_email_fkey" on table "public"."orders"`)
}

// A key the file removes is refused while a foreign key still in place
// references it, since PostgreSQL does not drop it and the plan would fail
// after its earlier statements committed. A foreign key on the same table
// that the plan drops ahead of the key no longer holds it.
func TestDiffRefusesDroppingAKeyAForeignKeyStillReferences(t *testing.T) {
	selfKey := Constraint{Name: "events_a_email_fkey", Def: "FOREIGN KEY (email) REFERENCES events(email)", ForeignKey: true}
	referenced := func(fk IncomingForeignKey, withSelfKey bool) Model {
		m := keyed()
		m.Constraints[1].ReferencedBy = []IncomingForeignKey{fk}
		if withSelfKey {
			m.Constraints = []Constraint{m.Constraints[0], selfKey, m.Constraints[1]}
		}
		return m
	}
	withoutKey := keyed()
	withoutKey.Constraints = withoutKey.Constraints[:1]

	t.Run("referenced from another table", func(t *testing.T) {
		_, err := Diff("public", referenced(IncomingForeignKey{Schema: "public", Table: "orders", Constraint: "orders_email_fkey"}, false), withoutKey)
		var refusal *UnsupportedChangeError
		require.True(t, errors.As(err, &refusal), "%v", err)
		assert.Equal(t, CauseReferencedIndex, refusal.Cause)
		assert.Contains(t, err.Error(), `constraint "events_email_key" is referenced by foreign key "orders_email_fkey" on table "public"."orders"`)
	})

	self := IncomingForeignKey{Schema: "public", Table: "events", Constraint: selfKey.Name}
	t.Run("referenced by a foreign key on the table that stays", func(t *testing.T) {
		desired := keyed()
		desired.Constraints = []Constraint{desired.Constraints[0], selfKey}
		_, err := Diff("public", referenced(self, true), desired)
		var refusal *UnsupportedChangeError
		require.True(t, errors.As(err, &refusal), "%v", err)
		assert.Equal(t, CauseReferencedIndex, refusal.Cause)
	})

	t.Run("referenced by a foreign key on the table the plan drops first", func(t *testing.T) {
		changes, err := Diff("public", referenced(self, true), withoutKey)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_a_email_fkey"`,
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key"`,
		}, sqls(changes))
	})
}
