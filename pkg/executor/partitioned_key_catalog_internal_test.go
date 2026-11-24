package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The resume decision's branches that the integration tests do not reach
// through the server, decided from catalog facts as readLeafKeyFacts returns
// them for a leaf app.events_2026_01 (OID 100) and the key (id, created_at).
func TestDecideLeafKey(t *testing.T) {
	leaf := partitionNode{oid: 100, parentOID: 10, schema: "app", name: "events_2026_01", relkind: "r"}
	columns := []leafKeyColumn{
		{name: "id", attnum: 1, notNull: true, quoted: "id"},
		{name: "created_at", attnum: 2, notNull: true, quoted: "created_at"},
	}
	const derived = "events_pkey_p100"
	// keyIndex is a valid unique index of the key's definition on the leaf.
	keyIndex := func(oid uint32, name string) relationFacts {
		return relationFacts{
			oid: oid, schema: "app", name: name, relkind: "i",
			tableOID: 100, tableName: "events_2026_01", tableRelkind: "r",
			valid: true, unique: true,
			def:        "CREATE UNIQUE INDEX " + name + " ON app.events_2026_01 USING btree (id, created_at)",
			quotedName: name, quotedSchema: "app", quotedTable: "events_2026_01",
		}
	}

	t.Run("a matching constraint already attached to a parent constraint is a conflict", func(t *testing.T) {
		idx := keyIndex(200, "events_2026_01_pkey")
		idx.backsConstraint = true
		facts := leafKeyFacts{columns: columns, constraints: []leafConstraint{
			{oid: 300, name: "events_2026_01_pkey", contype: "p", parentOID: 400, key: []int16{1, 2}, index: idx},
		}}

		_, err := decideLeafKey(eventsPrimaryKey, leaf, derived, facts)

		require.ErrorIs(t, err, ErrPartitionedKeyConflict)
	})

	t.Run("a primary key on other columns is a conflict", func(t *testing.T) {
		idx := keyIndex(200, "events_2026_01_pkey")
		idx.def = "CREATE UNIQUE INDEX events_2026_01_pkey ON app.events_2026_01 USING btree (id)"
		idx.backsConstraint = true
		facts := leafKeyFacts{columns: columns, constraints: []leafConstraint{
			{oid: 300, name: "events_2026_01_pkey", contype: "p", key: []int16{1}, index: idx},
		}}

		_, err := decideLeafKey(eventsPrimaryKey, leaf, derived, facts)

		require.ErrorIs(t, err, ErrPartitionedKeyConflict)
	})

	t.Run("a unique constraint on other columns does not stand in the way", func(t *testing.T) {
		idx := keyIndex(200, "events_2026_01_id_key")
		idx.def = "CREATE UNIQUE INDEX events_2026_01_id_key ON app.events_2026_01 USING btree (id)"
		idx.backsConstraint = true
		facts := leafKeyFacts{columns: columns, constraints: []leafConstraint{
			{oid: 300, name: "events_2026_01_id_key", contype: "u", key: []int16{1}, index: idx},
		}}

		plan, err := decideLeafKey(eventsPrimaryKey, leaf, derived, facts)

		require.NoError(t, err)
		assert.Equal(t, leafKeyPlan{state: leafKeyBuildNeeded}, plan)
	})

	t.Run("a constraint of another kind under the derived name takes the name", func(t *testing.T) {
		facts := leafKeyFacts{columns: columns, nameHeldByConstraint: true}

		_, err := decideLeafKey(eventsPrimaryKey, leaf, derived, facts)

		require.ErrorIs(t, err, ErrPartitionedKeyNameTaken)
	})

	t.Run("an invalid index of the key's definition under the derived name is debris", func(t *testing.T) {
		idx := keyIndex(200, derived)
		idx.valid = false
		facts := leafKeyFacts{columns: columns, occupant: idx, occupied: true}

		plan, err := decideLeafKey(eventsPrimaryKey, leaf, derived, facts)

		require.NoError(t, err)
		assert.Equal(t, leafKeyPlan{state: leafKeyBuildNeeded, debris: true}, plan)
	})

	t.Run("an index under the derived name attached to a partitioned index takes the name", func(t *testing.T) {
		idx := keyIndex(200, derived)
		idx.isPartition = true
		facts := leafKeyFacts{columns: columns, occupant: idx, occupied: true}

		_, err := decideLeafKey(eventsPrimaryKey, leaf, derived, facts)

		require.ErrorIs(t, err, ErrPartitionedKeyNameTaken)
	})

	t.Run("quarantined debris of an earlier recovery is swept before the build", func(t *testing.T) {
		facts := leafKeyFacts{columns: columns, quarantine: []invalidIndex{{oid: 500}}}

		plan, err := decideLeafKey(eventsPrimaryKey, leaf, derived, facts)

		require.NoError(t, err)
		assert.Equal(t, leafKeyPlan{state: leafKeyBuildNeeded, debris: true}, plan)
	})

	t.Run("a unique index of another definition is not reused", func(t *testing.T) {
		idx := keyIndex(200, "events_jan_desc")
		idx.def = "CREATE UNIQUE INDEX events_jan_desc ON app.events_2026_01 USING btree (id DESC, created_at)"
		facts := leafKeyFacts{columns: columns, reusable: []relationFacts{idx}}

		plan, err := decideLeafKey(eventsPrimaryKey, leaf, derived, facts)

		require.NoError(t, err)
		assert.Equal(t, leafKeyPlan{state: leafKeyBuildNeeded}, plan)
	})
}
