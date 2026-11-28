package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The attach's pairing, simulated from catalog facts for a parent and a table
// whose indexes all print the definition key below. The server walks the
// parent's indexes in OID order and gives each the first table index, in OID
// order, that is unattached, valid, and of the same definition, and that
// backs a constraint when the parent's index does (of the same type from
// PostgreSQL 17).
const pairingKey = "unique=true USING btree (id, day)"

func pairingParent(oid uint32, name, contype string) parentIndex {
	return parentIndex{facts: relationFacts{oid: oid, name: name}, key: pairingKey, contype: contype}
}

func pairingTable(oid uint32, contype string) attachTableIndex {
	return attachTableIndex{oid: oid, key: pairingKey, contype: contype, valid: true}
}

func TestPairAttachIndexes(t *testing.T) {
	// The plain parent index comes first and takes the table's
	// constraint-backed index, the lower OID, so the UNIQUE constraint
	// that follows finds only a plain index, which cannot back it.
	t.Run("a plain parent index takes a constraint-backed table index of a lower OID", func(t *testing.T) {
		s := attachIndexState{serverMajor: 17,
			parent: []parentIndex{pairingParent(1, "events_id_day_idx", ""), pairingParent(2, "events_id_day_key", "u")},
			table:  []attachTableIndex{pairingTable(10, "u"), pairingTable(20, "")},
		}
		assert.Equal(t, []uint32{10, 0}, pairAttachIndexes(s))
	})

	t.Run("the constraint first takes the constraint-backed index and the plain index the other", func(t *testing.T) {
		s := attachIndexState{serverMajor: 17,
			parent: []parentIndex{pairingParent(1, "events_id_day_key", "u"), pairingParent(2, "events_id_day_idx", "")},
			table:  []attachTableIndex{pairingTable(10, ""), pairingTable(20, "u")},
		}
		assert.Equal(t, []uint32{20, 10}, pairAttachIndexes(s))
	})

	t.Run("two identical parent indexes take one table index each", func(t *testing.T) {
		parent := []parentIndex{pairingParent(1, "events_a", ""), pairingParent(2, "events_b", "")}
		one := attachIndexState{serverMajor: 16, parent: parent, table: []attachTableIndex{pairingTable(10, "")}}
		assert.Equal(t, []uint32{10, 0}, pairAttachIndexes(one), "one table index goes to the first only")
		two := attachIndexState{serverMajor: 16, parent: parent, table: []attachTableIndex{pairingTable(10, ""), pairingTable(20, "")}}
		assert.Equal(t, []uint32{10, 20}, pairAttachIndexes(two))
	})

	// From PostgreSQL 17 the constraint types must agree; before it any
	// index-owning constraint on the table's index will do.
	t.Run("a parent primary key and a table unique constraint", func(t *testing.T) {
		parent := []parentIndex{pairingParent(1, "events_pkey", "p")}
		table := []attachTableIndex{pairingTable(10, "u")}
		assert.Equal(t, []uint32{0}, pairAttachIndexes(attachIndexState{serverMajor: 17, parent: parent, table: table}))
		assert.Equal(t, []uint32{10}, pairAttachIndexes(attachIndexState{serverMajor: 16, parent: parent, table: table}))
	})

	t.Run("a constraint-backed parent index never takes a plain table index", func(t *testing.T) {
		s := attachIndexState{serverMajor: 16,
			parent: []parentIndex{pairingParent(1, "events_pkey", "p")},
			table:  []attachTableIndex{pairingTable(10, "")},
		}
		assert.Equal(t, []uint32{0}, pairAttachIndexes(s))
	})

	t.Run("an attached, an invalid, an exclusion, and another definition's index are passed over", func(t *testing.T) {
		attached := pairingTable(10, "")
		attached.attached = true
		invalid := pairingTable(11, "")
		invalid.valid = false
		exclusion := pairingTable(12, "x")
		other := pairingTable(13, "")
		other.key = "unique=true USING btree (id)"
		s := attachIndexState{serverMajor: 17,
			parent: []parentIndex{pairingParent(1, "events_id_day_idx", "")},
			table:  []attachTableIndex{attached, invalid, exclusion, other, pairingTable(14, "")},
		}
		assert.Equal(t, []uint32{14}, pairAttachIndexes(s))
	})
}

// The read after the builds proves the attach builds nothing; anything else
// refuses with errAttachIndexIncomplete, which a rerun gets past by building
// what is missing.
func TestAttachIndexesComplete(t *testing.T) {
	parent := []parentIndex{pairingParent(1, "events_a", ""), pairingParent(2, "events_b", "")}
	results := []attachIndexResult{{parentIndex: "events_a", indexOID: 10}, {parentIndex: "events_b", indexOID: 20}}
	after := attachIndexState{serverMajor: 17, parent: parent,
		table: []attachTableIndex{pairingTable(10, ""), pairingTable(20, "")}}

	t.Run("every parent index pairs with its reported index", func(t *testing.T) {
		require.NoError(t, attachIndexesComplete(parent, after, results))
	})

	t.Run("a parent index added during the builds", func(t *testing.T) {
		grown := after
		grown.parent = append(append([]parentIndex{}, parent...), pairingParent(3, "events_c", ""))
		grown.table = append(append([]attachTableIndex{}, after.table...), pairingTable(30, ""))
		require.ErrorIs(t, attachIndexesComplete(parent, grown, results), errAttachIndexIncomplete)
	})

	// The table's indexes still pair one for one, but the parent's list is
	// not the one the step built for.
	t.Run("a parent index dropped and created again under a new OID", func(t *testing.T) {
		recreated := after
		recreated.parent = []parentIndex{parent[0], pairingParent(3, "events_b", "")}
		require.ErrorIs(t, attachIndexesComplete(parent, recreated, results), errAttachIndexIncomplete)
	})

	t.Run("a parent index left without a table index", func(t *testing.T) {
		short := after
		short.table = []attachTableIndex{pairingTable(10, "")}
		require.ErrorIs(t, attachIndexesComplete(parent, short, results), errAttachIndexIncomplete)
	})

	t.Run("a parent index paired with an index other than the reported one", func(t *testing.T) {
		swapped := []attachIndexResult{{parentIndex: "events_a", indexOID: 20}, {parentIndex: "events_b", indexOID: 10}}
		require.ErrorIs(t, attachIndexesComplete(parent, after, swapped), errAttachIndexIncomplete)
	})
}
