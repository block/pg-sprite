package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The relation under a plain parent index's derived name on the table
// app.events_2027 (OID 100): only an earlier run's build of the index the
// step would create is taken or rebuilt; anything else holds a name the step
// does not take from its owner.
func TestJudgeAttachIndexOccupant(t *testing.T) {
	table := standaloneTable(100, "app", "events_2027")
	pi := parentIndex{facts: relationFacts{oid: 1, name: "events_id_idx"}, key: "unique=false USING btree (id)"}
	const derived = "events_id_idx_p100"
	occupant := func() relationFacts {
		return relationFacts{
			oid: 200, schema: "app", name: derived, relkind: "i",
			tableOID: 100, tableName: "events_2027", tableRelkind: "r", valid: true,
			def:        "CREATE INDEX " + derived + " ON app.events_2027 USING btree (id)",
			quotedName: derived, quotedSchema: "app", quotedTable: "events_2027",
		}
	}

	t.Run("an earlier run's valid build is taken", func(t *testing.T) {
		got, debris, err := judgeAttachIndexOccupant(occupant(), pi, table, nil)
		require.NoError(t, err)
		assert.Equal(t, uint32(200), got.oid)
		assert.False(t, debris)
	})

	t.Run("an earlier run's invalid build is debris", func(t *testing.T) {
		occ := occupant()
		occ.valid = false
		got, debris, err := judgeAttachIndexOccupant(occ, pi, table, nil)
		require.NoError(t, err)
		assert.Zero(t, got.oid)
		assert.True(t, debris)
	})

	t.Run("an index attached to a partitioned index", func(t *testing.T) {
		occ := occupant()
		occ.isPartition = true
		_, _, err := judgeAttachIndexOccupant(occ, pi, table, nil)
		require.ErrorIs(t, err, ErrPartitionedIndexNameTaken)
	})

	t.Run("an index backing a constraint", func(t *testing.T) {
		occ := occupant()
		occ.backsConstraint = true
		_, _, err := judgeAttachIndexOccupant(occ, pi, table, nil)
		require.ErrorIs(t, err, ErrPartitionedIndexNameTaken)
	})

	t.Run("an index of another definition", func(t *testing.T) {
		occ := occupant()
		occ.def = "CREATE INDEX " + derived + " ON app.events_2027 USING btree (id DESC)"
		_, _, err := judgeAttachIndexOccupant(occ, pi, table, nil)
		require.ErrorIs(t, err, ErrPartitionedIndexNameTaken)
	})

	// The attach gives the index under this parent index's name to an
	// earlier parent index of the same definition, so it cannot count for
	// this one as well.
	t.Run("an index the attach gives another parent index", func(t *testing.T) {
		_, _, err := judgeAttachIndexOccupant(occupant(), pi, table, map[uint32]string{200: "events_id_a"})
		require.ErrorIs(t, err, ErrPartitionedIndexNameTaken)
	})
}
