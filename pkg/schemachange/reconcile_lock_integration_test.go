package schemachange_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A reconcile that loses the table lock stops where it is, slot commands
// included (LK-1). The drop of the lost slot waits for a walsender still
// streaming from it; the lock is terminated while it waits, and the
// reconcile returns the loss while the holder still holds — it does not
// wait the holder out and then drop a slot a run holding the lock since
// might own. The marker it wrote stays, so the next attempt reconciles
// from the top, and the slot is still there for it to drop.
func TestReconcileStopsWhenTheLockIsLostWhileTheDropWaits(t *testing.T) {
	f := newReconcileFixtureLocked(t, 100, dbconn.WithTableLockKeepalive(100*time.Millisecond))
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	c := newCopier(t, f, shadow)
	copyAll(t, f, c)
	row := f.row(shadow, checkpoint.PhaseCatchingUp, c.Position().Watermark, slot.Name(), slot.ConsistentPoint())
	require.NoError(t, f.store.Save(t.Context(), f.lock, row))
	saved := f.load(t, shadow)
	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{})
	require.NoError(t, err)

	release := f.holdSlot(t, slot)
	done := make(chan reconcileOutcome, 1)
	go func() {
		reconciled, err := reconciler.Reconcile(t.Context(), f.pool, saved)
		done <- reconcileOutcome{reconciled, err}
	}()
	f.awaitDropWaitingOnTheHolder(t, done)

	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, f.lock.BackendPID()).Scan(&terminated))
	require.True(t, terminated)
	const lockLossDeadline = 15 * time.Second
	select {
	case <-f.lock.Done():
	case <-time.After(lockLossDeadline):
		t.Fatalf("lock session did not report loss within %s", lockLossDeadline)
	}

	// The holder still streams; the reconcile must return anyway.
	const reconcileReturns = 10 * time.Second
	var got reconcileOutcome
	select {
	case got = <-done:
	case <-time.After(reconcileReturns):
		t.Fatalf("reconcile did not return within %s of the lock loss while the slot's holder still held it", reconcileReturns)
	}
	require.Error(t, got.err)
	assert.Equal(t, schemachange.CauseLockLost, schemachange.RefusalCauseOf(got.err))
	assert.ErrorIs(t, got.err, f.lock.Err(), "the loss the session reported is the cause")
	assert.Nil(t, got.reconciled.Slot)

	release()
	f.awaitSlotReleased(t, slot.Name())
	status, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	require.True(t, found, "the cancelled drop dropped nothing")
	assert.Equal(t, slot.ConsistentPoint(), status.ConfirmedFlushLSN, "the slot is the one the row names, not a replacement")
	stored := f.load(t, shadow)
	assert.Equal(t, checkpoint.PhaseReconciling, stored.Phase, "the marker stays for the next attempt")
	assert.Equal(t, slot.Name(), stored.SlotName)
	assert.Equal(t, slot.ConsistentPoint(), stored.LastAppliedLSN, "the row's position did not move")
	assert.True(t, stored.Watermark.Complete(), "the watermark survived")
}

// reconcileOutcome is what a Reconcile run in the background hands back.
type reconcileOutcome struct {
	reconciled schemachange.Reconciled
	err        error
}

// awaitDropWaitingOnTheHolder polls until the server shows the reconcile's
// drop waiting for the slot's holder: a walsender running
// DROP_REPLICATION_SLOT … WAIT. A reconcile that returns before then has
// not reached the drop, and the test says so.
func (f reconcileFixture) awaitDropWaitingOnTheHolder(t *testing.T, done <-chan reconcileOutcome) {
	t.Helper()
	const dropWaits = 15 * time.Second
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		select {
		case got := <-done:
			require.FailNowf(t, "reconcile returned before its drop waited on the holder", "%+v", got)
		default:
		}
		var waiting bool
		err := f.pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_stat_activity
				WHERE backend_type = 'walsender' AND query LIKE 'DROP_REPLICATION_SLOT %'
			)`).Scan(&waiting)
		if !assert.NoError(collect, err) {
			return
		}
		assert.True(collect, waiting, "the drop must be waiting on the server for the slot's holder")
	}, dropWaits, 50*time.Millisecond)
}
