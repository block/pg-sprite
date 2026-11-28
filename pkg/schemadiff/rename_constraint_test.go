package schemadiff

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renamedPrimaryKeyTable is keyed() with its primary key declared as
// events_id_pk over cols.
func renamedPrimaryKeyTable(def, indexDef string) Model {
	m := keyed()
	m.Constraints[0] = Constraint{Name: "events_id_pk", Def: def, Primary: true, IndexDef: indexDef}
	return m
}

// A primary key the file declares under another name keeps rejecting
// duplicates while the new key's index builds under a temporary name. One
// ALTER TABLE then drops the old key and attaches the new index, so the
// table always has a primary key, and a failed build leaves the old one in
// place.
func TestDiffSwapsInARenamedPrimaryKey(t *testing.T) {
	for name, tc := range map[string]struct {
		def, indexDef, build string
	}{
		"of the same columns": {
			def:      "PRIMARY KEY (id)",
			indexDef: "CREATE UNIQUE INDEX events_id_pk ON events USING btree (id)",
			build:    "CREATE UNIQUE INDEX events_id_pk_new ON public.events USING btree (id)",
		},
		"of other columns": {
			def:      "PRIMARY KEY (id, name)",
			indexDef: "CREATE UNIQUE INDEX events_id_pk ON events USING btree (id, name)",
			build:    "CREATE UNIQUE INDEX events_id_pk_new ON public.events USING btree (id, name)",
		},
	} {
		changes, err := Diff("public", keyed(), renamedPrimaryKeyTable(tc.def, tc.indexDef))
		require.NoError(t, err, name)
		require.Equal(t, []string{
			tc.build,
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_pkey", ADD CONSTRAINT "events_id_pk" PRIMARY KEY USING INDEX "events_id_pk_new"`,
		}, sqls(changes), name)
		swap := changes[1]
		assert.True(t, swap.Destructive, name)
		assert.Equal(t, &DroppedObject{Kind: DroppedConstraint, Name: "events_pkey", Definition: "PRIMARY KEY (id)"}, swap.Drops, name)
		require.NotNil(t, swap.Adopts, name)
		assert.Equal(t, "events_id_pk_new", swap.Adopts.Index, name)
	}
}

// A renamed primary key beside a dropped column is swapped in first, and
// the column is dropped last.
func TestDiffSwapsInARenamedPrimaryKeyBeforeDroppingAColumn(t *testing.T) {
	live := keyed()
	live.Columns = append(live.Columns, Column{Name: "legacy", Type: "text"})
	changes, err := Diff("public", live, renamedPrimaryKeyTable("PRIMARY KEY (id)", "CREATE UNIQUE INDEX events_id_pk ON events USING btree (id)"))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"CREATE UNIQUE INDEX events_id_pk_new ON public.events USING btree (id)",
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_pkey", ADD CONSTRAINT "events_id_pk" PRIMARY KEY USING INDEX "events_id_pk_new"`,
		`ALTER TABLE "public"."events" DROP COLUMN "legacy"`,
	}, sqls(changes))
}

// A renamed primary key a foreign key references is refused before
// anything is built: PostgreSQL does not drop a key a foreign key depends
// on.
func TestDiffRefusesARenamedPrimaryKeyAForeignKeyReferences(t *testing.T) {
	live := keyed()
	live.Constraints[0].ReferencedBy = []IncomingForeignKey{{Schema: "public", Table: "orders", Constraint: "orders_event_id_fkey"}}
	_, err := Diff("public", live, renamedPrimaryKeyTable("PRIMARY KEY (id)", "CREATE UNIQUE INDEX events_id_pk ON events USING btree (id)"))
	var refusal *UnsupportedChangeError
	require.True(t, errors.As(err, &refusal), "%v", err)
	assert.Equal(t, CauseReferencedIndex, refusal.Cause)
}

// A CHECK the file declares under another name with another rule keeps
// rejecting writes until the new rule is added, so rows the old rule
// forbids cannot get in while the new one is added and validated, and a
// new rule that fails validation leaves the old one standing.
func TestDiffAddsARenamedChangedCheckBeforeDroppingTheOld(t *testing.T) {
	live := keyed()
	live.Constraints = append(live.Constraints, Constraint{Name: "events_id_chk", Def: "CHECK (((id >= 0) AND (id < 100000000)))"})
	desired := keyed()
	desired.Constraints = append(desired.Constraints, Constraint{Name: "events_id_min_chk", Def: "CHECK ((id >= 0))"})

	t.Run("on its own", func(t *testing.T) {
		changes, err := Diff("public", live, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_id_min_chk" CHECK ((id >= 0))`,
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_id_chk"`,
		}, sqls(changes))
	})

	t.Run("beside a dropped column", func(t *testing.T) {
		withColumn := live
		withColumn.Columns = append(append([]Column(nil), live.Columns...), Column{Name: "legacy", Type: "text"})
		changes, err := Diff("public", withColumn, desired)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`ALTER TABLE "public"."events" ADD CONSTRAINT "events_id_min_chk" CHECK ((id >= 0))`,
			`ALTER TABLE "public"."events" DROP CONSTRAINT "events_id_chk"`,
			`ALTER TABLE "public"."events" DROP COLUMN "legacy"`,
		}, sqls(changes))
	})
}
