package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// base returns a small canonical model to mutate per test.
func base() Model {
	return Model{
		Table: "events",
		Columns: []Column{
			{Name: "id", Type: "bigint", NotNull: true},
			{Name: "name", Type: "character varying(50)", NotNull: true},
		},
		Constraints: []Constraint{
			{Name: "events_pkey", Def: "PRIMARY KEY (id)"},
		},
		Indexes: []Index{
			{Name: "events_name_idx", Def: "CREATE INDEX events_name_idx ON events USING btree (name)"},
		},
	}
}

func sqls(changes []Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.SQL
	}
	return out
}

func TestDiffNoChanges(t *testing.T) {
	changes, err := Diff("public", base(), base())
	require.NoError(t, err)
	assert.Empty(t, changes)
}

func TestDiffRefusesDifferentTables(t *testing.T) {
	other := base()
	other.Table = "users"
	_, err := Diff("public", base(), other)
	require.ErrorIs(t, err, ErrDifferentTables)
}

// Partitioning is table identity: no ALTER can change a partition key or a
// partition attachment in place, so a mismatch is a typed refusal — never a
// silent zero diff.
func TestDiffRefusesPartitioningMismatch(t *testing.T) {
	partitioned := base()
	partitioned.PartitionKey = "RANGE (id)"
	_, err := Diff("public", base(), partitioned)
	require.ErrorIs(t, err, ErrUnsupportedChange)

	child := base()
	child.IsPartition = true
	_, err = Diff("public", child, base())
	require.ErrorIs(t, err, ErrUnsupportedChange)
}

// Persistence converges only by a full table rewrite (SET LOGGED / SET
// UNLOGGED), which the engine does not derive — a mismatch is a typed
// refusal, never a silent zero diff.
func TestDiffRefusesPersistenceMismatch(t *testing.T) {
	unlogged := base()
	unlogged.Unlogged = true
	_, err := Diff("public", unlogged, base())
	require.ErrorIs(t, err, ErrUnsupportedChange)

	_, err = Diff("public", base(), unlogged)
	require.ErrorIs(t, err, ErrUnsupportedChange)
}

// Storage parameters are not converged: matching parameters diff to
// nothing, and any difference is a typed refusal, never a silent zero diff.
func TestDiffStorageParameters(t *testing.T) {
	tuned := base()
	tuned.Options = []string{"fillfactor=70"}
	changes, err := Diff("public", tuned, tuned)
	require.NoError(t, err)
	assert.Empty(t, changes)

	_, err = Diff("public", base(), tuned)
	require.ErrorIs(t, err, ErrUnsupportedChange)
	_, err = Diff("public", tuned, base())
	require.ErrorIs(t, err, ErrUnsupportedChange)
}

// An identity's sequence declaration is not converged, so a difference is
// a typed refusal; an added identity column carries its declaration in
// the emitted ADD COLUMN, so the column lands declared as the file says.
func TestDiffIdentitySequenceOptions(t *testing.T) {
	seq := &SequenceOptions{Type: "bigint", Start: 1000, Increment: 1, Min: 1, Max: 5000, Cache: 1}
	identity := func(s *SequenceOptions) Model {
		m := base()
		m.Columns = append(m.Columns, Column{Name: "ref", Type: "bigint", NotNull: true, Identity: IdentityAlways, Sequence: s})
		return m
	}

	_, err := Diff("public", identity(nil), identity(seq))
	require.ErrorIs(t, err, ErrUnsupportedChange)
	_, err = Diff("public", identity(seq), identity(nil))
	require.ErrorIs(t, err, ErrUnsupportedChange)
	changes, err := Diff("public", identity(seq), identity(&SequenceOptions{Type: "bigint", Start: 1000, Increment: 1, Min: 1, Max: 5000, Cache: 1}))
	require.NoError(t, err)
	assert.Empty(t, changes)

	changes, err = Diff("public", base(), identity(seq))
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, `ALTER TABLE "public"."events" ADD COLUMN "ref" bigint GENERATED ALWAYS AS IDENTITY (START WITH 1000 INCREMENT BY 1 MINVALUE 1 MAXVALUE 5000 CACHE 1 NO CYCLE) NOT NULL`, changes[0].SQL)

	mistyped := identity(&SequenceOptions{Type: "integer", Start: 1, Increment: 1, Min: 1, Max: 5000, Cache: 1})
	_, err = Diff("public", base(), mistyped)
	require.ErrorIs(t, err, ErrUnsupportedChange)
	require.ErrorIs(t, err, ErrUnrenderableSequenceOptions)
}

// A collation delta on an existing column rewrites the column, and an
// added column's COLLATE clause is not carried by the emitted ADD COLUMN —
// both are typed refusals, never a silent zero diff or a silently dropped
// clause.
func TestDiffRefusesCollationChanges(t *testing.T) {
	collated := base()
	collated.Columns[1].Collation = `"C"`
	_, err := Diff("public", base(), collated)
	require.ErrorIs(t, err, ErrUnsupportedChange)

	added := base()
	added.Columns = append(added.Columns, Column{
		Name: "note", Type: "text", Collation: `"C"`,
	})
	_, err = Diff("public", base(), added)
	require.ErrorIs(t, err, ErrUnsupportedChange)
}

// A type change on a column with an explicit collation carries the
// collation in its TYPE clause, so the retyped column keeps it: without the
// clause PostgreSQL gives the column the new type's default.
func TestDiffTypeChangeKeepsTheColumnsCollation(t *testing.T) {
	collated := func(typ string) Model {
		return Model{Table: "words", Columns: []Column{{Name: "tag", Type: typ, Collation: `pg_catalog."C"`}}}
	}
	changes, err := Diff("public", collated("character varying(20)"), collated("character varying(40)"))
	require.NoError(t, err)
	assert.Equal(t, []string{
		`ALTER TABLE "public"."words" ALTER COLUMN "tag" TYPE character varying(40) COLLATE pg_catalog."C"`,
	}, sqls(changes))
}

func TestDiffAddColumn(t *testing.T) {
	desired := base()
	desired.Columns = append(desired.Columns, Column{
		Name: "created_at", Type: "timestamp with time zone", NotNull: true, Default: "now()",
	})
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" ADD COLUMN "created_at" timestamp with time zone DEFAULT now() NOT NULL`,
	}, sqls(changes))
	assert.False(t, changes[0].Destructive)
}

// A rename expressed declaratively — the old column name gone, a new one
// present — is a drop plus an add, never an inferred rename: the differ
// does not heuristically pair columns, and the drop stays destructive so
// the caller gates it. Carrying the data over is expand/contract work
// (add, dual-write and backfill, switch reads, drop), not a diff.
func TestDiffRenameShapeIsDropPlusAdd(t *testing.T) {
	live := Model{
		Table: "users",
		Columns: []Column{
			{Name: "id", Type: "bigint", NotNull: true},
			{Name: "email", Type: "text"},
		},
	}
	desired := Model{
		Table: "users",
		Columns: []Column{
			{Name: "id", Type: "bigint", NotNull: true},
			{Name: "email_address", Type: "text"},
		},
	}
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."users" DROP COLUMN "email"`,
		`ALTER TABLE "public"."users" ADD COLUMN "email_address" text`,
	}, sqls(changes))
	assert.True(t, changes[0].Destructive)
	assert.False(t, changes[1].Destructive)
}

func TestDiffDropColumnIsDestructive(t *testing.T) {
	desired := base()
	desired.Columns = desired.Columns[:1] // drop "name"
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP COLUMN "name"`,
	}, sqls(changes))
	assert.True(t, changes[0].Destructive)
}

func TestDiffDropIndexIsDestructive(t *testing.T) {
	desired := base()
	desired.Indexes = nil
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`DROP INDEX "public"."events_name_idx"`,
	}, sqls(changes))
	assert.True(t, changes[0].Destructive)
	assert.Equal(t, ChangeDropIndex, changes[0].Kind)
}

func TestDiffRefusesSequenceDefaultAdoption(t *testing.T) {
	desired := base()
	desired.Columns[0].Default = "nextval('events_id_seq'::regclass)"
	desired.Columns[0].SequenceDefault = true
	_, err := Diff("public", base(), desired)
	require.ErrorIs(t, err, ErrUnsupportedChange)
}

func TestDiffRefusesSequenceDefaultOnAddedColumn(t *testing.T) {
	desired := base()
	desired.Columns = append(desired.Columns, Column{
		Name: "seq_col", Type: "integer",
		Default: "nextval('events_seq_col_seq'::regclass)", SequenceDefault: true,
	})
	_, err := Diff("public", base(), desired)
	require.ErrorIs(t, err, ErrUnsupportedChange)
}

func TestDiffIdenticalSequenceDefaultsConverge(t *testing.T) {
	withSerial := func() Model {
		m := base()
		m.Columns[0].Default = "nextval('events_id_seq'::regclass)"
		m.Columns[0].SequenceDefault = true
		return m
	}
	changes, err := Diff("public", withSerial(), withSerial())
	require.NoError(t, err)
	assert.Empty(t, changes)
}

func TestDiffChangeKinds(t *testing.T) {
	live := base()
	live.Columns = append(live.Columns, Column{Name: "legacy", Type: "integer"})

	desired := base()
	desired.Columns[1] = Column{Name: "name", Type: "text", NotNull: true}
	desired.Columns = append(desired.Columns, Column{Name: "email", Type: "text"})
	desired.Constraints = append(desired.Constraints, Constraint{
		Name: "events_email_key", Def: "UNIQUE (email)",
	})
	desired.Indexes = append(desired.Indexes, Index{
		Name: "events_email_idx", Def: "CREATE INDEX events_email_idx ON events USING btree (email)",
	})

	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	kinds := make([]ChangeKind, len(changes))
	for i, c := range changes {
		kinds[i] = c.Kind
	}
	assert.Equal(t, []ChangeKind{
		ChangeDropColumn,
		ChangeAddColumn,
		ChangeAlterType,
		ChangeAddConstraint,
		ChangeCreateIndex,
	}, kinds)
}

func TestDiffColumnAlterations(t *testing.T) {
	desired := base()
	desired.Columns[1] = Column{Name: "name", Type: "text", NotNull: false, Default: "'unnamed'::text"}
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	// The NOT NULL drop comes first: it is a drop, and a rewrite the type
	// change may run would change the attribute number its definition names.
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" ALTER COLUMN "name" DROP NOT NULL`,
		`ALTER TABLE "public"."events" ALTER COLUMN "name" TYPE text`,
		`ALTER TABLE "public"."events" ALTER COLUMN "name" SET DEFAULT 'unnamed'::text`,
	}, sqls(changes))
}

func TestDiffDropDefaultAndSetNotNull(t *testing.T) {
	live := base()
	live.Columns[1].Default = "'x'::character varying"
	live.Columns[1].NotNull = false
	changes, err := Diff("public", live, base())
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" ALTER COLUMN "name" DROP DEFAULT`,
		`ALTER TABLE "public"."events" ALTER COLUMN "name" SET NOT NULL`,
	}, sqls(changes))
}

func TestDiffConstraintChangeDropsAndReadds(t *testing.T) {
	desired := base()
	desired.Constraints = []Constraint{
		{Name: "events_pkey", Def: "PRIMARY KEY (id, name)"},
	}
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_pkey"`,
		`ALTER TABLE "public"."events" ADD CONSTRAINT "events_pkey" PRIMARY KEY (id, name)`,
	}, sqls(changes))
	assert.True(t, changes[0].Destructive)
	assert.False(t, changes[1].Destructive)
}

// A live constraint left NOT VALID with the desired definition, the state an
// interrupted online add leaves behind, converges by validating it: no drop,
// no re-add, nothing destructive. Validity is the only difference that earns
// this; a NOT VALID constraint whose definition also changed, or that the
// desired file no longer declares, is planned as before.
func TestDiffValidatesANotValidConstraintInPlace(t *testing.T) {
	check := Constraint{Name: "events_name_check", Def: "CHECK ((length((name)::text) > 0))"}
	fk := Constraint{Name: "events_owner_fkey", Def: "FOREIGN KEY (id) REFERENCES owners(id)", ForeignKey: true}
	notValid := func(c Constraint) Constraint {
		c.Def += " NOT VALID"
		c.NotValid = true
		return c
	}
	with := func(cons ...Constraint) Model {
		m := base()
		m.Constraints = append(m.Constraints, cons...)
		return m
	}

	t.Run("check constraint", func(t *testing.T) {
		changes, err := Diff("public", with(notValid(check)), with(check))
		require.NoError(t, err)
		require.Len(t, changes, 1)
		assert.Equal(t, `ALTER TABLE "public"."events" VALIDATE CONSTRAINT "events_name_check"`, changes[0].SQL)
		assert.Equal(t, ChangeValidateConstraint, changes[0].Kind)
		assert.False(t, changes[0].Destructive)
		assert.False(t, changes[0].Kind.IsForeignKey())
		assert.Equal(t, &ConstraintValidation{Constraint: check.Name, Def: check.Def + " NOT VALID"}, changes[0].Validates,
			"execution checks the live definition the diff matched")
	})

	t.Run("foreign key", func(t *testing.T) {
		changes, err := Diff("public", with(notValid(fk)), with(fk))
		require.NoError(t, err)
		require.Len(t, changes, 1)
		assert.Equal(t, `ALTER TABLE "public"."events" VALIDATE CONSTRAINT "events_owner_fkey"`, changes[0].SQL)
		assert.Equal(t, ChangeValidateForeignKey, changes[0].Kind)
		assert.False(t, changes[0].Destructive)
		assert.True(t, changes[0].Kind.IsForeignKey(), "validating a foreign key is a foreign-key change")
		assert.Equal(t, &ConstraintValidation{Constraint: fk.Name, Def: fk.Def + " NOT VALID"}, changes[0].Validates)
	})

	t.Run("definition also changed", func(t *testing.T) {
		changed := Constraint{Name: check.Name, Def: "CHECK ((length((name)::text) > 1))"}
		changes, err := Diff("public", with(notValid(check)), with(changed))
		require.NoError(t, err)
		require.Equal(t, []string{
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_name_check", ADD CONSTRAINT "events_name_check" CHECK ((length((name)::text) > 1)) NOT VALID`,
			`ALTER TABLE "public"."events" VALIDATE CONSTRAINT "events_name_check"`,
		}, sqls(changes))
		assert.True(t, changes[0].Destructive)
		assert.Equal(t, &DroppedObject{Kind: DroppedConstraint, Name: check.Name, Definition: check.Def + " NOT VALID"}, changes[0].Drops)
	})

	t.Run("no longer declared", func(t *testing.T) {
		changes, err := Diff("public", with(notValid(check)), base())
		require.NoError(t, err)
		require.Equal(t, []string{`ALTER TABLE "public"."events" DROP CONSTRAINT "events_name_check"`}, sqls(changes))
		assert.True(t, changes[0].Destructive)
	})

	t.Run("validated on both sides", func(t *testing.T) {
		changes, err := Diff("public", with(check), with(check))
		require.NoError(t, err)
		assert.Empty(t, changes)
	})
}

// An added primary key or unique constraint records the index it owns, under
// its own name, so a plan that builds that index and attaches it in a later
// step can check that the index under that name is still the one the build
// left. A CHECK owns no index and records none.
func TestDiffAddedKeyRecordsItsIndex(t *testing.T) {
	base := func() Model {
		return Model{Table: "events", Columns: []Column{{Name: "id", Type: "bigint", NotNull: true}, {Name: "name", Type: "text"}}}
	}
	for _, con := range []Constraint{
		{Name: "events_pkey", Def: "PRIMARY KEY (id)", Primary: true, IndexDef: "CREATE UNIQUE INDEX events_pkey ON events USING btree (id)"},
		{Name: "events_name_key", Def: "UNIQUE (name)", IndexDef: "CREATE UNIQUE INDEX events_name_key ON events USING btree (name)"},
	} {
		desired := base()
		desired.Constraints = []Constraint{con}
		changes, err := Diff("public", base(), desired)
		require.NoError(t, err)
		require.Len(t, changes, 1, con.Name)
		assert.Equal(t, &IndexAdoption{Index: con.Name, Def: con.IndexDef}, changes[0].AddsKey, con.Name)
		assert.Nil(t, changes[0].Adopts, "%s: the add itself takes over no live index", con.Name)
	}

	desired := base()
	desired.Constraints = []Constraint{{Name: "events_name_check", Def: "CHECK ((length(name) > 0))"}}
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Nil(t, changes[0].AddsKey)
}

// An added CHECK or FOREIGN KEY records the definition it has once added NOT
// VALID, so a plan that validates it in a later step can check that the
// constraint under that name is still the one the add created. A changed
// constraint is replaced NOT VALID in one statement, and the validation that
// follows it records the same definition.
func TestDiffAddedConstraintRecordsItsNotValidDefinition(t *testing.T) {
	check := Constraint{Name: "events_name_check", Def: "CHECK ((length((name)::text) > 0))"}
	fk := Constraint{Name: "events_owner_fkey", Def: "FOREIGN KEY (id) REFERENCES owners(id)", ForeignKey: true}
	with := func(cons ...Constraint) Model {
		m := base()
		m.Constraints = append(m.Constraints, cons...)
		return m
	}

	t.Run("check constraint", func(t *testing.T) {
		changes, err := Diff("public", base(), with(check))
		require.NoError(t, err)
		require.Len(t, changes, 1)
		assert.Equal(t, &ConstraintValidation{Constraint: check.Name, Def: check.Def + " NOT VALID"}, changes[0].AddsNotValid)
		assert.Nil(t, changes[0].Validates, "the add itself validates nothing that already exists")
	})

	t.Run("foreign key", func(t *testing.T) {
		changes, err := Diff("public", base(), with(fk))
		require.NoError(t, err)
		require.Len(t, changes, 1)
		assert.Equal(t, &ConstraintValidation{Constraint: fk.Name, Def: fk.Def + " NOT VALID"}, changes[0].AddsNotValid)
	})

	t.Run("changed constraint", func(t *testing.T) {
		changed := Constraint{Name: check.Name, Def: "CHECK ((length((name)::text) > 1))"}
		changes, err := Diff("public", with(check), with(changed))
		require.NoError(t, err)
		require.Len(t, changes, 2)
		assert.Nil(t, changes[0].AddsNotValid, "the replacement already adds NOT VALID")
		assert.Equal(t, &ConstraintValidation{Constraint: check.Name, Def: changed.Def + " NOT VALID"}, changes[1].Validates)
	})

	t.Run("unique constraint", func(t *testing.T) {
		unique := Constraint{Name: "events_name_key", Def: "UNIQUE (name)"}
		changes, err := Diff("public", base(), with(unique))
		require.NoError(t, err)
		require.NotEmpty(t, changes)
		for _, ch := range changes {
			assert.Nil(t, ch.AddsNotValid, "a key cannot be added NOT VALID: %s", ch.SQL)
		}
	})
}

// A new unique constraint whose name and index match a live unique index
// takes the index over in place rather than dropping it and building its
// own, which would leave the table without the guarantee and without the
// index while the build runs. Every shape the server would not adopt, or
// that would not come out the same as a fresh constraint, keeps the drop
// and rebuild.
func TestDiffKeyConstraintAdoptsTheLiveIndex(t *testing.T) {
	const indexDef = "CREATE UNIQUE INDEX events_name_key ON events USING btree (name)"
	live := func(ix Index) Model {
		m := base()
		m.Indexes = []Index{ix}
		return m
	}
	desired := func(con Constraint) Model {
		m := base()
		m.Indexes = nil
		m.Constraints = append(m.Constraints, con)
		return m
	}
	unique := Constraint{Name: "events_name_key", Def: "UNIQUE (name)", IndexDef: indexDef}
	rebuild := []string{
		`DROP INDEX "public"."events_name_key"`,
		`ALTER TABLE "public"."events" ADD CONSTRAINT "events_name_key" UNIQUE (name)`,
	}

	t.Run("a unique constraint", func(t *testing.T) {
		changes, err := Diff("public", live(Index{Name: "events_name_key", Def: indexDef}), desired(unique))
		require.NoError(t, err)
		require.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_name_key" UNIQUE USING INDEX "events_name_key"`,
		}, sqls(changes))
		assert.False(t, changes[0].Destructive)
		assert.Equal(t, ChangeAddConstraint, changes[0].Kind)
		assert.Equal(t, &IndexAdoption{Index: "events_name_key", Def: indexDef}, changes[0].Adopts,
			"execution checks the index against the definition the diff matched")
	})

	t.Run("a primary key", func(t *testing.T) {
		const pkDef = "CREATE UNIQUE INDEX events_pkey ON events USING btree (id)"
		l := base()
		l.Constraints = nil
		l.Indexes = []Index{{Name: "events_pkey", Def: pkDef}}
		d := base()
		d.Indexes = nil
		d.Constraints = []Constraint{{Name: "events_pkey", Def: "PRIMARY KEY (id)", Primary: true, IndexDef: pkDef}}
		changes, err := Diff("public", l, d)
		require.NoError(t, err)
		require.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_pkey" PRIMARY KEY USING INDEX "events_pkey"`,
		}, sqls(changes))
	})

	t.Run("an index of another definition", func(t *testing.T) {
		other := Index{Name: "events_name_key", Def: "CREATE UNIQUE INDEX events_name_key ON events USING btree (name) WHERE (id > 0)"}
		changes, err := Diff("public", live(other), desired(unique))
		require.NoError(t, err)
		assert.Equal(t, rebuild, sqls(changes))
	})

	t.Run("an unfinished build", func(t *testing.T) {
		changes, err := Diff("public", live(Index{Name: "events_name_key", Def: indexDef, Invalid: true}), desired(unique))
		require.NoError(t, err)
		assert.Equal(t, rebuild[1:], sqls(changes), "the unfinished build is never dropped here, and never adopted")
	})

	t.Run("a deferrable constraint", func(t *testing.T) {
		deferrable := unique
		deferrable.Def = "UNIQUE (name) DEFERRABLE"
		deferrable.Deferrable = true
		changes, err := Diff("public", live(Index{Name: "events_name_key", Def: indexDef}), desired(deferrable))
		require.NoError(t, err)
		require.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_name_key" UNIQUE USING INDEX "events_name_key" DEFERRABLE`,
		}, sqls(changes))
		assert.Equal(t, &IndexAdoption{Index: "events_name_key", Def: indexDef}, changes[0].Adopts)
	})

	t.Run("an initially deferred constraint", func(t *testing.T) {
		deferred := unique
		deferred.Def = "UNIQUE (name) DEFERRABLE INITIALLY DEFERRED"
		deferred.Deferrable, deferred.InitiallyDeferred = true, true
		changes, err := Diff("public", live(Index{Name: "events_name_key", Def: indexDef}), desired(deferred))
		require.NoError(t, err)
		require.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_name_key" UNIQUE USING INDEX "events_name_key" DEFERRABLE INITIALLY DEFERRED`,
		}, sqls(changes))
		assert.Equal(t, &IndexAdoption{Index: "events_name_key", Def: indexDef}, changes[0].Adopts)
	})

	t.Run("a partitioned parent", func(t *testing.T) {
		l := live(Index{Name: "events_name_key", Def: indexDef})
		l.PartitionKey = "RANGE (id)"
		d := desired(unique)
		d.PartitionKey = "RANGE (id)"
		changes, err := Diff("public", l, d)
		require.NoError(t, err)
		assert.Equal(t, rebuild, sqls(changes))
	})
}

// A live index that matches the desired definition but is invalid is an
// unfinished concurrent build: it does not deliver the desired index, so
// the diff emits the create — and only the create, never a blocking DROP
// INDEX of an entry whose builder may still be running.
func TestDiffInvalidIndexRebuildsWithoutDrop(t *testing.T) {
	live := base()
	live.Indexes[0].Invalid = true
	changes, err := Diff("public", live, base())
	require.NoError(t, err)
	require.Equal(t, []string{
		`CREATE INDEX events_name_idx ON public.events USING btree (name)`,
	}, sqls(changes))
	assert.Equal(t, ChangeCreateIndex, changes[0].Kind)
	assert.False(t, changes[0].Destructive)
}

// An invalid live index that desired redefines is rebuilt to the new
// definition by the create alone; the unfinished build under the old
// definition is not dropped, for the same reason.
func TestDiffInvalidIndexRedefinedCreatesWithoutDrop(t *testing.T) {
	live := base()
	live.Indexes[0].Invalid = true
	desired := base()
	desired.Indexes[0].Def = "CREATE UNIQUE INDEX events_name_idx ON events USING btree (name)"
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`CREATE UNIQUE INDEX events_name_idx ON public.events USING btree (name)`,
	}, sqls(changes))
	assert.Equal(t, ChangeCreateIndex, changes[0].Kind)
	assert.False(t, changes[0].Destructive)
}

// An invalid live index that desired no longer names is left alone — the
// leftover of a REINDEX ... CONCURRENTLY, or a build someone else is
// running, is not the diff's to remove — while a valid index in the same
// position is dropped as usual.
func TestDiffInvalidIndexAbsentFromDesiredIsLeftAlone(t *testing.T) {
	live := base()
	live.Indexes = append(live.Indexes,
		Index{Name: "events_name_idx_ccnew", Def: "CREATE INDEX events_name_idx_ccnew ON events USING btree (name)", Invalid: true},
		Index{Name: "events_legacy_idx", Def: "CREATE INDEX events_legacy_idx ON events USING btree (id)"},
	)
	changes, err := Diff("public", live, base())
	require.NoError(t, err)
	require.Equal(t, []string{
		`DROP INDEX "public"."events_legacy_idx"`,
	}, sqls(changes))
	assert.Equal(t, ChangeDropIndex, changes[0].Kind)
}

// On a partitioned parent an invalid index means unattached partition
// indexes. One whose definition matches the desired one is an unfinished
// partition-by-partition build, so the same CREATE INDEX is planned and the
// partitioned index build resumes it; one the desired file no longer names is
// dropped, as for any index. Nothing is dropped for the one that matches.
func TestDiffPartitionedParentResumesAnInvalidIndexWithTheDesiredDefinition(t *testing.T) {
	live := base()
	live.PartitionKey = "RANGE (id)"
	live.Indexes[0].Invalid = true
	desired := base()
	desired.PartitionKey = "RANGE (id)"

	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`CREATE INDEX events_name_idx ON public.events USING btree (name)`,
	}, sqls(changes))
	assert.Equal(t, ChangeCreateIndex, changes[0].Kind)
	assert.False(t, changes[0].Destructive)

	desired.Indexes = nil
	changes, err = Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`DROP INDEX "public"."events_name_idx"`,
	}, sqls(changes))
}

// A valid index on a partitioned parent with the desired definition is
// delivered: nothing is planned for it.
func TestDiffPartitionedParentLeavesAValidIndexWithTheDesiredDefinition(t *testing.T) {
	live := base()
	live.PartitionKey = "RANGE (id)"
	desired := base()
	desired.PartitionKey = "RANGE (id)"

	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Empty(t, changes)
}

func TestDiffOrderingDropsBeforeAddsBeforeIndexes(t *testing.T) {
	live := base()
	live.Columns = append(live.Columns, Column{Name: "legacy", Type: "integer"})

	desired := base()
	desired.Columns = append(desired.Columns, Column{Name: "email", Type: "text", NotNull: true})
	desired.Indexes = append(desired.Indexes, Index{
		Name: "events_email_idx", Def: "CREATE INDEX events_email_idx ON events USING btree (email)",
	})

	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP COLUMN "legacy"`,
		`ALTER TABLE "public"."events" ADD COLUMN "email" text NOT NULL`,
		`CREATE INDEX events_email_idx ON public.events USING btree (email)`,
	}, sqls(changes))
}

func TestDiffRefusesIdentityAndGeneratedChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Column)
	}{
		{"identity change", func(c *Column) { c.Identity = IdentityAlways }},
		{"generated change", func(c *Column) { c.Generated = true; c.Default = "(id + 1)" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired := base()
			tt.mutate(&desired.Columns[0])
			_, err := Diff("public", base(), desired)
			require.ErrorIs(t, err, ErrUnsupportedChange)
		})
	}
}

func TestDiffIdentityColumnAddRendersIdentity(t *testing.T) {
	desired := base()
	desired.Columns = append(desired.Columns,
		Column{Name: "seq_a", Type: "bigint", NotNull: true, Identity: IdentityAlways},
		Column{Name: "seq_d", Type: "bigint", NotNull: true, Identity: IdentityByDefault},
	)
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" ADD COLUMN "seq_a" bigint GENERATED ALWAYS AS IDENTITY NOT NULL`,
		`ALTER TABLE "public"."events" ADD COLUMN "seq_d" bigint GENERATED BY DEFAULT AS IDENTITY NOT NULL`,
	}, sqls(changes))
}

func TestDiffGeneratedColumnAddRendersGenerationExpression(t *testing.T) {
	desired := base()
	desired.Columns = append(desired.Columns, Column{
		Name: "name_upper", Type: "text", Generated: true, Default: "upper((name)::text)",
	})
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" ADD COLUMN "name_upper" text GENERATED ALWAYS AS (upper((name)::text)) STORED`,
	}, sqls(changes))
}

// A stored generated column whose expression changed is planned as one SET
// EXPRESSION statement carrying the desired expression, so the drift shows in
// the plan instead of failing it. A type change on a generated column still
// fails the diff.
func TestDiffGeneratedColumnExpressionChange(t *testing.T) {
	live := base()
	live.Columns = append(live.Columns, Column{
		Name: "name_upper", Type: "text", Generated: true, Default: "upper((name)::text)",
	})
	desired := base()
	desired.Columns = append(desired.Columns, Column{
		Name: "name_upper", Type: "text", Generated: true, Default: "lower((name)::text)",
	})
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []Change{{
		SQL:  `ALTER TABLE "public"."events" ALTER COLUMN "name_upper" SET EXPRESSION AS (lower((name)::text))`,
		Kind: ChangeSetGeneratedExpression,
	}}, changes)

	desired.Columns[len(desired.Columns)-1].Type = "character varying"
	_, err = Diff("public", live, desired)
	require.ErrorIs(t, err, ErrUnsupportedChange)
	assert.Contains(t, err.Error(), `column "name_upper" generated column type change`)
}

// A live virtual generated column that the desired file declares any other
// way fails the diff instead of converging the expression as a default. Two
// virtual models whose expression differs plan the same refused SET
// EXPRESSION a stored column does.
func TestDiffVirtualGeneratedColumn(t *testing.T) {
	live := base()
	live.Columns = append(live.Columns, Column{Name: "doubled", Type: "integer", Virtual: true, Default: "(id * 2)"})

	stored := base()
	stored.Columns = append(stored.Columns, Column{Name: "doubled", Type: "integer", Generated: true, Default: "(id * 2)"})
	_, err := Diff("public", live, stored)
	require.ErrorIs(t, err, ErrUnsupportedChange)
	assert.Contains(t, err.Error(), `column "doubled" generated change`)

	plain := base()
	plain.Columns = append(plain.Columns, Column{Name: "doubled", Type: "integer", Default: "3"})
	_, err = Diff("public", live, plain)
	require.ErrorIs(t, err, ErrUnsupportedChange)
	assert.Contains(t, err.Error(), `column "doubled" virtual generated change`)

	virtual := base()
	virtual.Columns = append(virtual.Columns, Column{Name: "doubled", Type: "integer", Virtual: true, Default: "(id * 3)"})
	changes, err := Diff("public", live, virtual)
	require.NoError(t, err)
	require.Equal(t, []Change{{
		SQL:  `ALTER TABLE "public"."events" ALTER COLUMN "doubled" SET EXPRESSION AS ((id * 3))`,
		Kind: ChangeSetGeneratedExpression,
	}}, changes)
}

// The change-kind vocabulary is a closed set a consumer branches on; it is
// pinned to the plan report's format_version (docs/plan-report.md). A new
// value here without a format_version bump is a contract break.
func TestChangeKindsVocabularyPinned(t *testing.T) {
	assert.Equal(t, []ChangeKind{
		ChangeCreateTable,
		ChangeDropIndex,
		ChangeDropConstraint,
		ChangeDropForeignKey,
		ChangeDropColumn,
		ChangeAddColumn,
		ChangeAlterType,
		ChangeSetGeneratedExpression,
		ChangeSetDefault,
		ChangeDropDefault,
		ChangeSetNotNull,
		ChangeDropNotNull,
		ChangeAddConstraint,
		ChangeAddForeignKey,
		ChangeValidateConstraint,
		ChangeValidateForeignKey,
		ChangeCreateIndex,
		ChangeRenameIndex,
		ChangeAddEnumValue,
	}, ChangeKinds())
}

// Every NOT NULL drop runs with the drops, after the column drops and before
// anything that alters, adds or rewrites: a rewrite renumbers the table's
// columns, and a NOT NULL drop is reviewed by the column number it has when
// the plan is derived. This holds for a NOT NULL on a column other than the
// one a later statement retypes.
func TestDiffDropsNotNullsAfterColumnDropsAndBeforeAlters(t *testing.T) {
	live := base()
	live.Columns = append(live.Columns,
		Column{Name: "legacy", Type: "integer"},
		Column{Name: "qty", Type: "integer", NotNull: true})

	desired := base()
	desired.Columns[1] = Column{Name: "name", Type: "text", NotNull: true}
	desired.Columns = append(desired.Columns,
		Column{Name: "qty", Type: "integer"},
		Column{Name: "email", Type: "text"})

	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP COLUMN "legacy"`,
		`ALTER TABLE "public"."events" ALTER COLUMN "qty" DROP NOT NULL`,
		`ALTER TABLE "public"."events" ADD COLUMN "email" text`,
		`ALTER TABLE "public"."events" ALTER COLUMN "name" TYPE text`,
	}, sqls(changes))
	require.NotNil(t, changes[1].Drops)
	assert.Equal(t, DroppedNotNull, changes[1].Drops.Kind)
}

// A dropped column's reviewed definition names its collation, and the plan
// lists what the drop removes with it: the objects that depend on the column,
// in the order the introspection returned them.
func TestDiffDropColumnCarriesCollationAndDependents(t *testing.T) {
	live := base()
	live.Columns = append(live.Columns, Column{
		Name: "legacy", Type: "text", Collation: `"C"`, AttNum: 3,
		Dependents: []Dependent{
			{Kind: "index", Name: "events_legacy_idx", Def: "CREATE INDEX events_legacy_idx ON public.events USING btree (legacy)"},
			{Kind: "owned sequence", Name: "public.events_legacy_seq"},
		},
	})

	changes, err := Diff("public", live, base())
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.NotNil(t, changes[0].Drops)
	assert.Equal(t, DroppedColumn, changes[0].Drops.Kind)
	assert.Contains(t, changes[0].Drops.Definition, `COLLATE "C"`)
	assert.Equal(t, []string{
		"index events_legacy_idx: CREATE INDEX events_legacy_idx ON public.events USING btree (legacy)",
		"owned sequence public.events_legacy_seq",
	}, changes[0].Drops.Dependents)
}

// A dependent the plan did not list is one the reviewer has not seen; one the
// plan listed that has since gone is not.
func TestUnlistedDependents(t *testing.T) {
	planned := []string{"index a", "constraint b"}
	assert.Empty(t, UnlistedDependents(planned, []string{"constraint b"}), "a dependent gone since planning is fine")
	assert.Empty(t, UnlistedDependents(planned, nil))
	assert.Equal(t, []string{"index c"}, UnlistedDependents(planned, []string{"index a", "index c"}))
	assert.Equal(t, []string{"constraint b: CHECK (x > 1)"}, UnlistedDependents(planned, []string{"constraint b: CHECK (x > 1)"}),
		"a dependent whose definition changed is a new one")
}
