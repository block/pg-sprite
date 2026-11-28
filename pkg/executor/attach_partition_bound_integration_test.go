package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// These tests run the attach step where its target and the catalog disagree
// with the reviewed statement's bound: proofs derived from another bound,
// and a table already attached under another bound.

// The proofs are validated, but derived from a wider bound than the
// statement's: a CHECK for 2027 and 2028 does not imply the 2027 bound, so
// the ATTACH would scan the table under its locks. The step refuses, typed,
// and the table stays standalone.
func TestAttachPartitionOnlineRefusesProofsOfAnotherBound(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsAttachParent)
		wider, ok, err := statement.AttachedPartition(`ALTER TABLE events
			ATTACH PARTITION events_2027
			FOR VALUES FROM ('2027-01-01') TO ('2029-01-01')`)
		require.NoError(t, err)
		require.True(t, ok)
		target := f.attachTarget(t, attachProofBound, attachProofDefault)
		check, exclusion, err := deriveAttachBoundCheck(t.Context(), briefTx{pool: f.pool}, target.parentOID, wider.Bound)
		require.NoError(t, err)
		target.bound = f.proofOn(t, attachProofBound, "events_2027", check)
		target.deflt = f.proofOn(t, attachProofDefault, "events_default", exclusion)
		for _, p := range target.proofs() {
			_, err := ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, 0, nil, nil)
			require.NoError(t, err)
		}

		_, err = attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
		require.ErrorIs(t, err, errAttachProofsNotOfTheBound)
		assert.Empty(t, f.partitionBound(t, "events_2027"))
	})
}

// Someone attached events_2027 to the parent by hand, for 2027 and 2028,
// after the run proved the statement's 2027 bound. The table is a partition
// of the parent, but not as reviewed: the step refuses, typed, and leaves
// the partition and both proofs for an operator to decide on.
func TestAttachPartitionOnlineRefusesATableAttachedUnderAnotherBound(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsAttachParent)
		target := f.attachTarget(t)
		f.exec(t, `ALTER TABLE %[1]s.events
			ATTACH PARTITION %[1]s.events_2027
			FOR VALUES FROM ('2027-01-01') TO ('2029-01-01')`)

		res, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
		require.ErrorIs(t, err, errAttachedUnderAnotherBound)
		assert.NotEqual(t, attachPartitionFound, res.action)
		assert.Equal(t, "FOR VALUES FROM ('2027-01-01') TO ('2029-01-01')", f.partitionBound(t, "events_2027"))
		assert.Len(t, f.proofConstraints(t, "events_2027"), 1)
		assert.Len(t, f.proofConstraints(t, "events_default"), 1)
	})
}
