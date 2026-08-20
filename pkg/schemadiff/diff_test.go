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
	require.Equal(t, []string{
		`ALTER TABLE "public"."events" ALTER COLUMN "name" TYPE text`,
		`ALTER TABLE "public"."events" ALTER COLUMN "name" SET DEFAULT 'unnamed'::text`,
		`ALTER TABLE "public"."events" ALTER COLUMN "name" DROP NOT NULL`,
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

func TestDiffIndexChangeDropsAndRecreatesQualified(t *testing.T) {
	desired := base()
	desired.Indexes = []Index{
		{Name: "events_name_idx", Def: "CREATE UNIQUE INDEX events_name_idx ON events USING btree (name)"},
	}
	changes, err := Diff("public", base(), desired)
	require.NoError(t, err)
	require.Equal(t, []string{
		`DROP INDEX "public"."events_name_idx"`,
		`CREATE UNIQUE INDEX events_name_idx ON public.events USING btree (name)`,
	}, sqls(changes))
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

func TestDiffRefusesGenerationExpressionChange(t *testing.T) {
	live := base()
	live.Columns[0] = Column{Name: "id", Type: "bigint", Generated: true, Default: "(1)"}
	desired := base()
	desired.Columns[0] = Column{Name: "id", Type: "bigint", Generated: true, Default: "(2)"}
	_, err := Diff("public", live, desired)
	require.ErrorIs(t, err, ErrUnsupportedChange)
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

// The change-kind vocabulary is a closed set a consumer branches on; it is
// pinned to plan-report format_version 1 (docs/plan-report.md). A new value
// here without a format_version bump is a contract break.
func TestChangeKindsVocabularyPinned(t *testing.T) {
	assert.Equal(t, []ChangeKind{
		ChangeCreateTable,
		ChangeDropIndex,
		ChangeDropConstraint,
		ChangeDropColumn,
		ChangeAddColumn,
		ChangeAlterType,
		ChangeSetDefault,
		ChangeDropDefault,
		ChangeSetNotNull,
		ChangeDropNotNull,
		ChangeAddConstraint,
		ChangeCreateIndex,
	}, ChangeKinds())
}
