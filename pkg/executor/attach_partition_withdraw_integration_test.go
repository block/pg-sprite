package executor

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests stop the online attach at its ATTACH, after the DEFAULT
// partition's proof is in place, and check what the stopped run leaves.

// failAtTheAttach runs the online attach of orders_2027 while another
// session holds SHARE on the parent alone. Nothing the run does before the
// ATTACH locks the parent in a mode that conflicts with it; the ATTACH's
// SHARE UPDATE EXCLUSIVE on the parent does, so the ATTACH runs out of its
// lock budget after every proof, CHECK, index, and foreign key is in place.
// The holder is released before failAtTheAttach returns.
func (f attachProofFixture) failAtTheAttach(t *testing.T) AttachPartitionReport {
	t.Helper()
	holder, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = holder.Exec(t.Context(), "LOCK TABLE ONLY "+pgx.Identifier{f.schema, "orders"}.Sanitize()+" IN SHARE MODE")
	require.NoError(t, err)
	rep, runErr := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
	require.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
	require.Error(t, runErr)
	assert.Equal(t, CodeBudgetLockExceeded, OutcomeCode(runErr), "%v", runErr)
	return rep
}

// An ATTACH that does not commit leaves the DEFAULT partition without its
// proof, so the writes the parent routes there inside the new bound go on,
// and the drop is in the report. The new table keeps everything it was
// given, its bound proof included, and the rerun attaches it.
func TestAttachPartitionOnlineDropsTheDefaultProofWhenTheAttachDoesNotCommit(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)

		rep := f.failAtTheAttach(t)
		require.Len(t, rep.Steps, 10, "%+v", rep.Steps)
		proof := rep.Steps[8].Name
		assert.Equal(t, []AttachPartitionStep{
			{Kind: AttachPartitionStepDefaultProof, Name: proof, Action: "added"},
			{Kind: AttachPartitionStepDefaultProof, Name: proof, Action: "dropped"},
		}, rep.Steps[8:])
		assert.False(t, rep.Attached)
		assert.Empty(t, f.partitionBound(t, "orders_2027"))
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
		assert.Equal(t, []string{rep.Steps[0].Name}, f.proofConstraints(t, "orders_2027"))

		// 2027-06-01 is inside the new bound; with the table not attached,
		// the parent routes it to the DEFAULT partition, which takes it.
		f.exec(t, `INSERT INTO %[1]s.orders VALUES (9, '2027-06-01', 'eu', 1, 10)`)
		var inDefault int
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, "orders_default"}.Sanitize()+" WHERE id = 9").Scan(&inDefault))
		assert.Equal(t, 1, inDefault)
		f.exec(t, `DELETE FROM %[1]s.orders WHERE id = 9`)

		again, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.NoError(t, err)
		assert.True(t, again.Attached)
		assert.Equal(t, AttachPartitionStep{Kind: AttachPartitionStepBoundProof, Name: rep.Steps[0].Name, Action: "found"}, again.Steps[0])
		assert.Equal(t, AttachPartitionStep{Kind: AttachPartitionStepDefaultProof, Name: proof, Action: "added"}, again.Steps[len(again.Steps)-1])
		assert.Equal(t, orders2027Bound, f.partitionBound(t, "orders_2027"))
		assert.Empty(t, f.proofConstraints(t, "orders_2027"))
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
	})
}

// A table someone attached after a stopped run still carries that run's
// bound proof. The rerun finds the table attached, drops the proof, and
// reports the drop, so a cleanup is told apart from a rerun with nothing
// to do.
func TestAttachPartitionOnlineReportsTheProofsItDropsFromATableFoundAttached(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		rep := f.failAtTheAttach(t)
		boundProof := rep.Steps[0].Name
		f.exec(t, `ALTER TABLE %[1]s.orders
			ATTACH PARTITION %[1]s.orders_2027
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)

		again, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.NoError(t, err)
		assert.False(t, again.Attached)
		assert.Equal(t, orders2027Bound, again.Bound)
		assert.Equal(t, []AttachPartitionStep{
			{Kind: AttachPartitionStepBoundProof, Name: boundProof, Action: "dropped"},
		}, again.Steps)
		assert.Empty(t, f.proofConstraints(t, "orders_2027"))
	})
}
