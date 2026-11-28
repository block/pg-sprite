package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
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

// attachQueuedDeadline bounds the wait for the run to reach its ATTACH and
// queue behind the session holding the parent.
const attachQueuedDeadline = 30 * time.Second

// stopAtTheAttach runs the online attach of orders_2027 while another
// session holds SHARE on the parent alone, as failAtTheAttach does, but
// under an ATTACH lock budget the test never reaches: once the ATTACH is
// queued behind the holder, with every proof in place, it calls whileQueued
// and then cancels the run's context. The holder is released before
// stopAtTheAttach returns.
func (f attachProofFixture) stopAtTheAttach(t *testing.T, whileQueued func()) (AttachPartitionReport, error) {
	t.Helper()
	orders := pgx.Identifier{f.schema, "orders"}.Sanitize()
	holder, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { assert.NoError(t, holder.Rollback(context.WithoutCancel(t.Context()))) }()
	_, err = holder.Exec(t.Context(), "LOCK TABLE ONLY "+orders+" IN SHARE MODE")
	require.NoError(t, err)
	pt, err := preflight.CheckTable(t.Context(), f.pool, f.schema, "orders", preflight.NoSizeLimit)
	require.NoError(t, err)
	o := attachFlowOptions
	o.Attach = Budget{LockTimeout: time.Minute, StatementTimeout: time.Minute}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var rep AttachPartitionReport
	var runErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		rep, runErr = AttachPartitionOnline(ctx, dbconn.Config{URL: f.url}, f.pool, pt, fmt.Sprintf(attachOrders2027Online, f.schema), o)
	})
	// The ATTACH's SHARE UPDATE EXCLUSIVE on the parent is the one lock the
	// run requests there, and the holder's SHARE keeps it from being granted.
	queued := assert.Eventually(t, func() bool {
		var waiting bool
		err := f.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE relation = $1::regclass AND mode = 'ShareUpdateExclusiveLock' AND NOT granted)`, orders).Scan(&waiting)
		return err == nil && waiting
	}, attachQueuedDeadline, 10*time.Millisecond)
	if queued {
		whileQueued()
	}
	cancel()
	wg.Wait()
	require.True(t, queued, "the ATTACH never queued behind the holder: %v", runErr)
	return rep, runErr
}

// A caller that cancels while the ATTACH waits on the parent gets the
// cancel back, and the DEFAULT partition's proof is still dropped: the drop
// runs under a context the cancel does not reach.
func TestAttachPartitionOnlineDropsTheDefaultProofWhenTheCallerCancels(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)

		rep, err := f.stopAtTheAttach(t, func() {})
		require.ErrorIs(t, err, context.Canceled)
		require.Len(t, rep.Steps, 10, "%+v", rep.Steps)
		proof := rep.Steps[8].Name
		assert.Equal(t, []AttachPartitionStep{
			{Kind: AttachPartitionStepDefaultProof, Name: proof, Action: "added"},
			{Kind: AttachPartitionStepDefaultProof, Name: proof, Action: "dropped"},
		}, rep.Steps[8:])
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
		assert.Empty(t, f.partitionBound(t, "orders_2027"))
	})
}

// When the DEFAULT partition's proof cannot be dropped, here because a
// second session holds SHARE on the DEFAULT partition past the drop's lock
// budget, the error carries the CHECK, the DEFAULT partition, and the
// statement that drops it as fields, and that statement drops it.
func TestAttachPartitionOnlineNamesTheDefaultProofItCouldNotDrop(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		proof := f.defaultProof(t)
		var blocker pgx.Tx
		defer func() {
			if blocker != nil {
				assert.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
			}
		}()

		_, err := f.stopAtTheAttach(t, func() {
			var err error
			blocker, err = f.pool.Begin(t.Context())
			require.NoError(t, err)
			_, err = blocker.Exec(t.Context(), "LOCK TABLE ONLY "+pgx.Identifier{f.schema, "orders_default"}.Sanitize()+" IN SHARE MODE")
			require.NoError(t, err)
		})
		require.ErrorIs(t, err, context.Canceled)
		left, ok := errors.AsType[*AttachDefaultProofLeftError](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, f.schema+".orders_default", left.DefaultPartition)
		assert.Equal(t, proof.name(), left.Constraint)
		assert.Equal(t, proof.dropSQL(), left.DropSQL)
		assert.Equal(t, f.schema+".orders_2027", left.Table)
		assert.Equal(t, f.schema+".orders", left.Parent)
		assert.True(t, left.Seen)
		assert.Equal(t, CauseLock, budgetCause(t, left.Err))
		assert.Equal(t, []string{proof.name()}, f.proofConstraints(t, "orders_default"))

		require.NoError(t, blocker.Rollback(t.Context()))
		blocker = nil
		_, err = f.pool.Exec(t.Context(), left.DropSQL)
		require.NoError(t, err)
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
	})
}

// budgetCause is the cause of the *BudgetError err wraps.
func budgetCause(t *testing.T, err error) BudgetCause {
	t.Helper()
	budgetErr, ok := errors.AsType[*BudgetError](err)
	require.True(t, ok, "%v", err)
	return budgetErr.Cause
}

// The withdrawal keeps the DEFAULT partition's proof when the table is a
// partition of the parent: here a stopped run gave the table everything,
// someone attached it by hand, and an earlier run's proof stayed on the
// DEFAULT partition. With the table attached the proof refuses nothing the
// parent routes there, and a rerun that finds the table attached drops it.
func TestAttachPartitionWithdrawalKeepsTheDefaultProofOfAnAttachedTable(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		f.failAtTheAttach(t)
		f.exec(t, `ALTER TABLE %[1]s.orders
			ATTACH PARTITION %[1]s.orders_2027
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
		proof := f.leaveDefaultProof(t)
		check, _, err := f.derive(t, attachOrders2027Online)
		require.NoError(t, err)
		attach, ok, err := statement.AttachedPartition(fmt.Sprintf(attachOrders2027Online, f.schema))
		require.NoError(t, err)
		require.True(t, ok)
		rep := AttachPartitionReport{}
		flow := attachPartitionFlow{pool: f.pool, attach: attach, opts: attachFlowOptions, rep: &rep}
		target := attachPartitionTarget{
			parentOID: f.oid(t, "orders"),
			bound:     attachProof{kind: attachProofBound, tableOID: f.oid(t, "orders_2027"), schema: f.schema, table: "orders_2027", expr: check},
			deflt:     proof,
		}
		stopped := errors.New("the run stopped")

		err = flow.withdrawDefaultProof(t.Context(), target, stopped)
		assert.Same(t, stopped, err)
		assert.Empty(t, rep.Steps)
		assert.Equal(t, []string{proof.name()}, f.proofConstraints(t, "orders_default"))
	})
}
