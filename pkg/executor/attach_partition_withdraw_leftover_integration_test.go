package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests rerun the online attach after a run that died between the
// DEFAULT partition's proof and the ATTACH, which leaves that proof in
// place, and check that a rerun refused before it adds anything drops it;
// and that a refusal with no proof to drop leaves the DEFAULT partition
// alone.

// oid is the table's OID.
func (f attachProofFixture) oid(t *testing.T, table string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT $1::regclass::oid",
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&oid))
	return oid
}

// defaultProof is the proof the online attach of orders_2027 gives the
// DEFAULT partition: the CHECK that excludes the 2027 bound.
func (f attachProofFixture) defaultProof(t *testing.T) attachProof {
	t.Helper()
	_, exclusion, err := f.derive(t, attachOrders2027Online)
	require.NoError(t, err)
	return attachProof{kind: attachProofDefault, tableOID: f.oid(t, "orders_default"), schema: f.schema, table: "orders_default", expr: exclusion}
}

// leaveDefaultProof gives the DEFAULT partition the validated proof a run
// that died before its ATTACH leaves there, under the name the flow derives.
func (f attachProofFixture) leaveDefaultProof(t *testing.T) attachProof {
	t.Helper()
	proof := f.defaultProof(t)
	f.prove(t, "orders_default", proof.name(), proof.expr)
	require.Equal(t, []string{proof.name()}, f.proofConstraints(t, "orders_default"))
	return proof
}

// A rerun refused because the caller did not accept the DEFAULT partition's
// write window drops the proof an earlier run left there, so the writes it
// refuses go on, and reports the drop. The table gets nothing.
func TestAttachPartitionOnlineDropsALeftoverDefaultProofWhenTheWindowIsRefused(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		proof := f.leaveDefaultProof(t)
		o := attachFlowOptions
		o.AcceptDefaultWriteWindow = false

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, o, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionDefaultWindow)
		assert.Equal(t, CodeAttachPartitionDefaultWindow, OutcomeCode(err))
		assert.Equal(t, []AttachPartitionStep{
			{Kind: AttachPartitionStepDefaultProof, Name: proof.name(), Action: "dropped"},
		}, rep.Steps)
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
		f.requireNothingAdded(t, "orders_2027")
	})
}

// A rerun refused because the table's columns no longer match the parent's
// (a note column was added since the earlier run) is refused before the
// flow reads anything else about the table, and still drops the proof the
// earlier run left on the DEFAULT partition.
func TestAttachPartitionOnlineDropsALeftoverDefaultProofWhenTheTableIsRefused(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		proof := f.leaveDefaultProof(t)
		f.exec(t, `ALTER TABLE %[1]s.orders_2027 ADD COLUMN note text`)

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionTable)
		assert.Equal(t, CodeAttachPartitionTable, OutcomeCode(err))
		assert.Equal(t, []AttachPartitionStep{
			{Kind: AttachPartitionStepDefaultProof, Name: proof.name(), Action: "dropped"},
		}, rep.Steps)
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
		f.requireNothingAdded(t, "orders_2027")
	})
}

// A rerun whose table is gone is refused, and still drops the proof the
// earlier run left on the DEFAULT partition: with no table under the
// statement's name, nothing is attached there.
func TestAttachPartitionOnlineDropsALeftoverDefaultProofWhenTheTableIsGone(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		proof := f.leaveDefaultProof(t)
		f.exec(t, `DROP TABLE %[1]s.orders_2027`)

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionTable)
		assert.Equal(t, CodeAttachPartitionTable, OutcomeCode(err))
		assert.Equal(t, []AttachPartitionStep{
			{Kind: AttachPartitionStepDefaultProof, Name: proof.name(), Action: "dropped"},
		}, rep.Steps)
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
	})
}

// A refusal with no proof on the DEFAULT partition neither waits for nor
// locks it: here a second session holds SHARE on the DEFAULT partition
// across the run, which the withdrawal's lock conflicts with, and the run
// still returns the refusal itself, not a failed withdrawal.
func TestAttachPartitionOnlineRefusalWithoutAProofLeavesTheDefaultPartitionAlone(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		holder, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { assert.NoError(t, holder.Rollback(context.WithoutCancel(t.Context()))) }()
		_, err = holder.Exec(t.Context(), "LOCK TABLE ONLY "+pgx.Identifier{f.schema, "orders_default"}.Sanitize()+" IN SHARE MODE")
		require.NoError(t, err)
		o := attachFlowOptions
		o.AcceptDefaultWriteWindow = false

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, o, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionDefaultWindow)
		assert.Equal(t, CodeAttachPartitionDefaultWindow, OutcomeCode(err))
		_, left := errors.AsType[*AttachDefaultProofLeftError](err)
		assert.False(t, left, "%v", err)
		assert.Empty(t, rep.Steps)
		f.requireNothingAdded(t, "orders_2027")
	})
}
