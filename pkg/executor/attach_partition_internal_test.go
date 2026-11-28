package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// The attach step's decisions that need no server: what a target must hold,
// the order the ATTACH locks the proofs' tables in, what the table is a
// child of, and which of the table's foreign keys the ATTACH adopts.

const (
	unitParentOID  = 100
	unitTableOID   = 200
	unitDefaultOID = 300
)

func unitAttachTarget(t *testing.T) attachPartitionTarget {
	t.Helper()
	st, err := statement.ParseOne(`ALTER TABLE app.events
		ATTACH PARTITION app.events_2027
		FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
	require.NoError(t, err)
	return attachPartitionTarget{
		st:         st,
		searchPath: []string{"app", "public"},
		parentOID:  unitParentOID,
		bound:      attachProof{kind: attachProofBound, tableOID: unitTableOID, schema: "app", table: "events_2027", expr: "bound check"},
		deflt:      attachProof{kind: attachProofDefault, tableOID: unitDefaultOID, schema: "app", table: "events_default", expr: "default exclusion"},
	}
}

func TestAttachPartitionTargetValidate(t *testing.T) {
	require.NoError(t, unitAttachTarget(t).validate())

	withoutDefault := unitAttachTarget(t)
	withoutDefault.deflt = attachProof{}
	require.NoError(t, withoutDefault.validate(), "a parent without a DEFAULT partition needs only the table's proof")

	for _, tc := range []struct {
		name   string
		change func(t *testing.T, target *attachPartitionTarget)
	}{
		{"no parent", func(_ *testing.T, target *attachPartitionTarget) { target.parentOID = 0 }},
		{"no search_path", func(_ *testing.T, target *attachPartitionTarget) { target.searchPath = nil }},
		{"the table's proof is a DEFAULT partition proof", func(_ *testing.T, target *attachPartitionTarget) {
			target.bound.kind = attachProofDefault
		}},
		{"the DEFAULT partition's proof is a bound proof", func(_ *testing.T, target *attachPartitionTarget) {
			target.deflt.kind = attachProofBound
		}},
		{"a proof without an expression", func(_ *testing.T, target *attachPartitionTarget) { target.deflt.expr = "" }},
		{"another statement", func(t *testing.T, target *attachPartitionTarget) {
			st, err := statement.ParseOne(`ALTER TABLE app.events DETACH PARTITION app.events_2027`)
			require.NoError(t, err)
			target.st = st
		}},
		{"a DEFAULT attach", func(t *testing.T, target *attachPartitionTarget) {
			st, err := statement.ParseOne(`ALTER TABLE app.events ATTACH PARTITION app.events_2027 DEFAULT`)
			require.NoError(t, err)
			target.st = st
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := unitAttachTarget(t)
			tc.change(t, &target)
			require.ErrorIs(t, target.validate(), ErrInvariantViolation)
		})
	}
}

// The proofs are dropped table first; the ATTACH locks the DEFAULT
// partition before the table.
func TestAttachPartitionTargetProofsAndLockOrder(t *testing.T) {
	target := unitAttachTarget(t)
	assert.True(t, target.hasDefault())
	assert.Equal(t, []attachProof{target.bound, target.deflt}, target.proofs())
	assert.Equal(t, []attachProof{target.deflt, target.bound}, target.lockOrder())

	target.deflt = attachProof{}
	assert.False(t, target.hasDefault())
	assert.Equal(t, []attachProof{target.bound}, target.proofs())
	assert.Equal(t, []attachProof{target.bound}, target.lockOrder())
}

func TestAttachPartitionTargetRequireProofsOf(t *testing.T) {
	target := unitAttachTarget(t)
	require.NoError(t, target.requireProofsOf("bound check", "default exclusion"))
	require.ErrorIs(t, target.requireProofsOf("another check", "default exclusion"), errAttachProofsNotOfTheBound)
	require.ErrorIs(t, target.requireProofsOf("bound check", "another exclusion"), errAttachProofsNotOfTheBound)

	target.deflt = attachProof{}
	require.NoError(t, target.requireProofsOf("bound check", "any exclusion"), "without a DEFAULT partition only the table's proof is compared")
}

func TestAttachMembership(t *testing.T) {
	p := unitAttachTarget(t).bound
	for _, tc := range []struct {
		name        string
		m           attachMembership
		partitionOf bool
		standalone  bool
	}{
		{"standalone", attachMembership{}, false, true},
		{"a partition of the parent", attachMembership{isPartition: true, parents: []uint32{unitParentOID}}, true, false},
		{"a partition of another parent", attachMembership{isPartition: true, parents: []uint32{999}}, false, false},
		{"detach-pending from the parent", attachMembership{isPartition: true, parents: []uint32{unitParentOID}, detachPending: true}, false, false},
		{"an inheritance child of the parent", attachMembership{parents: []uint32{unitParentOID}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.partitionOf, tc.m.isPartitionOf(unitParentOID))
			err := tc.m.requireStandalone(p)
			if tc.standalone {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrInvariantViolation)
			}
		})
	}
}

// The parent's foreign key on account_id (its column 3) references accounts
// (OID 50) id (column 1); the table numbers account_id as its column 1.
func unitParentFK(oid uint32) catalogFK {
	return catalogFK{oid: oid, name: "events_account_id_fkey", refOID: 50, key: []int16{3}, refKey: []int16{1},
		match: "s", onUpdate: "a", onDelete: "a", validated: true}
}

func unitTableFK(oid uint32, validated bool) catalogFK {
	return catalogFK{oid: oid, refOID: 50, key: []int16{1}, refKey: []int16{1},
		match: "s", onUpdate: "a", onDelete: "a", validated: validated}
}

func TestPairAttachForeignKeys(t *testing.T) {
	keys := [][]int16{{1}}
	parent := []catalogFK{unitParentFK(1)}

	t.Run("a validated foreign key of the definition", func(t *testing.T) {
		assert.Equal(t, []uint32{10}, pairAttachForeignKeys(parent, keys, []catalogFK{unitTableFK(10, true)}))
	})

	t.Run("none", func(t *testing.T) {
		assert.Equal(t, []uint32{0}, pairAttachForeignKeys(parent, keys, nil))
	})

	t.Run("a NOT VALID one only", func(t *testing.T) {
		assert.Equal(t, []uint32{0}, pairAttachForeignKeys(parent, keys, []catalogFK{unitTableFK(10, false)}))
	})

	// The server may try the NOT VALID one first, and from PostgreSQL 18
	// it would validate that one under the attach's locks.
	t.Run("a NOT VALID one beside a validated one", func(t *testing.T) {
		table := []catalogFK{unitTableFK(10, true), unitTableFK(20, false)}
		assert.Equal(t, []uint32{0}, pairAttachForeignKeys(parent, keys, table))
	})

	t.Run("one already attached to a parent's", func(t *testing.T) {
		attached := unitTableFK(10, true)
		attached.parentOID = 7
		assert.Equal(t, []uint32{0}, pairAttachForeignKeys(parent, keys, []catalogFK{attached}))
	})

	t.Run("one of another definition", func(t *testing.T) {
		cascade := unitTableFK(10, true)
		cascade.onDelete = "c"
		assert.Equal(t, []uint32{0}, pairAttachForeignKeys(parent, keys, []catalogFK{cascade}))
		otherColumn := unitTableFK(10, true)
		otherColumn.key = []int16{2}
		assert.Equal(t, []uint32{0}, pairAttachForeignKeys(parent, keys, []catalogFK{otherColumn}))
	})

	t.Run("two identical foreign keys of the parent take one each", func(t *testing.T) {
		two := []catalogFK{unitParentFK(1), unitParentFK(2)}
		twoKeys := [][]int16{{1}, {1}}
		assert.Equal(t, []uint32{10, 0}, pairAttachForeignKeys(two, twoKeys, []catalogFK{unitTableFK(10, true)}))
		assert.Equal(t, []uint32{10, 20}, pairAttachForeignKeys(two, twoKeys, []catalogFK{unitTableFK(10, true), unitTableFK(20, true)}))
	})
}
