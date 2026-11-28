package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A plan that drops a column and changes a key, check, or foreign key on the
// columns that remain replaces the rule first and drops the column last.
// Dropping the column first would take the old rule with it, and writes would
// go unchecked by either rule until the new one was in place.
func TestDiffReplacesRulesBeforeDroppingAColumn(t *testing.T) {
	tenant := Column{Name: "tenant", Type: "bigint", NotNull: true}
	email := Column{Name: "email", Type: "text"}

	t.Run("a check that read the dropped column", func(t *testing.T) {
		live, desired := base(), base()
		live.Columns = append(live.Columns, Column{Name: "legacy", Type: "integer"})
		live.Constraints = append(live.Constraints, Constraint{Name: "events_rule", Def: "CHECK (((id > 0) AND (legacy > 0)))"})
		desired.Constraints = append(desired.Constraints, Constraint{Name: "events_rule", Def: "CHECK ((id > 0))"})
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_rule", ADD CONSTRAINT "events_rule" CHECK ((id > 0)) NOT VALID`,
			`ALTER TABLE "public"."events" VALIDATE CONSTRAINT "events_rule"`,
			`ALTER TABLE "public"."events" DROP COLUMN "legacy"`,
		}, sqls(changes))
	})

	t.Run("a unique key that loses the dropped column", func(t *testing.T) {
		live, desired := base(), base()
		live.Columns = append(live.Columns, email, tenant)
		desired.Columns = append(desired.Columns, email)
		live.Constraints = append(live.Constraints, Constraint{
			Name: "events_email_key", Def: "UNIQUE (email, tenant)",
			IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email, tenant)",
		})
		desired.Constraints = append(desired.Constraints, Constraint{
			Name: "events_email_key", Def: "UNIQUE (email)",
			IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email)",
		})
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			"CREATE UNIQUE INDEX events_email_key_new ON public.events USING btree (email)",
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key", ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key_new"`,
			`ALTER TABLE "public"."events" DROP COLUMN "tenant"`,
		}, sqls(changes))
	})

	t.Run("a primary key that loses the dropped column", func(t *testing.T) {
		live, desired := base(), base()
		live.Columns = append(live.Columns, tenant)
		live.Constraints = []Constraint{{
			Name: "events_pkey", Def: "PRIMARY KEY (id, tenant)", Primary: true,
			IndexDef: "CREATE UNIQUE INDEX events_pkey ON events USING btree (id, tenant)",
		}}
		desired.Constraints = []Constraint{{
			Name: "events_pkey", Def: "PRIMARY KEY (id)", Primary: true,
			IndexDef: "CREATE UNIQUE INDEX events_pkey ON events USING btree (id)",
		}}
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			"CREATE UNIQUE INDEX events_pkey_new ON public.events USING btree (id)",
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_pkey", ADD CONSTRAINT "events_pkey" PRIMARY KEY USING INDEX "events_pkey_new"`,
			`ALTER TABLE "public"."events" DROP COLUMN "tenant"`,
		}, sqls(changes))
	})

	t.Run("a foreign key that loses the dropped column", func(t *testing.T) {
		live, desired := base(), base()
		live.Columns = append(live.Columns, Column{Name: "pid", Type: "bigint"}, Column{Name: "t", Type: "bigint"})
		desired.Columns = append(desired.Columns, Column{Name: "pid", Type: "bigint"})
		live.Constraints = append(live.Constraints, Constraint{Name: "events_parent_fk", Def: "FOREIGN KEY (pid, t) REFERENCES parents(id, t)", ForeignKey: true})
		desired.Constraints = append(desired.Constraints, Constraint{Name: "events_parent_fk", Def: "FOREIGN KEY (pid) REFERENCES parents(id)", ForeignKey: true})
		assert.Empty(t, ForeignKeysDroppedBeforeTheirReplacement(live, desired),
			"the old key holds until the new one replaces it")
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE public.events DROP CONSTRAINT events_parent_fk, ADD CONSTRAINT events_parent_fk FOREIGN KEY (pid) REFERENCES public.parents (id) NOT VALID`,
			`ALTER TABLE "public"."events" VALIDATE CONSTRAINT "events_parent_fk"`,
			`ALTER TABLE "public"."events" DROP COLUMN "t"`,
		}, sqls(changes))
	})
}

// A column drop beside an added column that may rewrite the table keeps the
// drops first: a rewrite renumbers the columns, and the dropped column is
// reviewed by the number it had when the plan was derived.
func TestDiffDropsAColumnFirstBesideAnAddThatMayRewrite(t *testing.T) {
	tenant := Column{Name: "tenant", Type: "bigint", NotNull: true}
	email := Column{Name: "email", Type: "text"}
	live, desired := base(), base()
	live.Columns = append(live.Columns, email, tenant)
	desired.Columns = append(desired.Columns, email, Column{Name: "created", Type: "timestamp with time zone", Default: "now()"})
	live.Constraints = append(live.Constraints, Constraint{
		Name: "events_email_key", Def: "UNIQUE (email, tenant)",
		IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email, tenant)",
	})
	desired.Constraints = append(desired.Constraints, Constraint{
		Name: "events_email_key", Def: "UNIQUE (email)",
		IndexDef: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email)",
	})
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_email_key"`,
		`ALTER TABLE "public"."events" DROP COLUMN "tenant"`,
		`ALTER TABLE "public"."events" ADD COLUMN "created" timestamp with time zone DEFAULT now()`,
		`ALTER TABLE "public"."events" ADD CONSTRAINT "events_email_key" UNIQUE (email)`,
	}, sqls(changes))
}

func TestChangesColumnsFirst(t *testing.T) {
	withLegacy := base()
	withLegacy.Columns = append(withLegacy.Columns, Column{Name: "legacy", Type: "integer"})
	retyped := base()
	retyped.Columns[1].Type = "text"
	add := func(m Model, c Column) []Column { return append(append([]Column{}, m.Columns...), c) }

	assert.False(t, changesColumnsFirst(base(), base().Columns), "no column change")
	assert.False(t, changesColumnsFirst(withLegacy, base().Columns), "a drop alone")
	assert.False(t, changesColumnsFirst(withLegacy, add(base(), Column{Name: "note", Type: "text"})), "a drop beside a plain add")
	assert.True(t, changesColumnsFirst(base(), retyped.Columns), "a retype")
	assert.True(t, changesColumnsFirst(withLegacy, add(base(), Column{Name: "note", Type: "text", Default: "'x'::text"})), "a drop beside a defaulted add")
	assert.True(t, changesColumnsFirst(withLegacy, add(base(), Column{Name: "total", Type: "integer", Default: "(id * 2)", Generated: true})), "a drop beside a generated add")
	assert.True(t, changesColumnsFirst(withLegacy, add(base(), Column{Name: "seq", Type: "bigint", Identity: IdentityAlways})), "a drop beside an identity add")
	assert.False(t, changesColumnsFirst(base(), add(base(), Column{Name: "note", Type: "text", Default: "'x'::text"})), "a defaulted add with no drop")
}
