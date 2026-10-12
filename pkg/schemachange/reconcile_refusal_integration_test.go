package schemachange_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests here are the refusals and the durable marker of reconcile mode:
// what a reconcile must not touch, and what it leaves behind when it fails.

// swapUnderTheNames renames the tables the way a committed cutover leaves
// them: the source becomes the retained old table and the shadow takes the
// source's name.
func (f reconcileFixture) swapUnderTheNames(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	schema := f.table.Schema
	source := pgx.Identifier{schema, f.table.Table}.Sanitize()
	old := pgx.Identifier{schemachange.OldName(schema, f.table.Table)}.Sanitize()
	shadowName := pgx.Identifier{schema, shadow.ShadowTable()}.Sanitize()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, source, old))
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, shadowName, pgx.Identifier{f.table.Table}.Sanitize()))
	require.NoError(t, err)
}

func (f reconcileFixture) assertNoSlot(t *testing.T) {
	t.Helper()
	_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found, "no refusal made a slot")
}

// A row in the cutover phase is refused before anything is read or
// written: its swap may have committed, and the shadow's name may then be
// on the live table, so neither a resume nor a reconcile acts on it until
// InspectSwapped has answered. The row is left as found and no slot is
// made, whether the swap committed or not.
func TestReconcileRefusesARowWhoseSwapCommitted(t *testing.T) {
	f := newReconcileFixture(t, 200)
	shadow := f.buildShadow(t)
	copyAll(t, f, newCopier(t, f, shadow))
	cp := f.row(shadow, checkpoint.PhaseCutover, copier.NewWatermark(math.MaxInt64), f.target.DecodingName(), 0)
	require.NoError(t, f.store.Save(t.Context(), f.lock, cp))
	f.swapUnderTheNames(t, shadow)

	_, err := schemachange.InspectResume(t.Context(), f.pool, f.target, cp)
	assert.Equal(t, schemachange.CauseResumeCutover, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(t.Context(), f.pool, cp)
	assert.Equal(t, schemachange.CauseResumeCutover, schemachange.RefusalCauseOf(err))

	assert.Equal(t, checkpoint.PhaseCutover, f.load(t, shadow).Phase, "the refusal wrote nothing")
	f.assertNoSlot(t)
}

// The cutover refusal is answered before the slot is read, so a cutover
// row whose swap rolled back and whose slot was lost in the same window
// has its way forward through the caller: InspectSwapped says the swap
// did not happen, the caller writes the row back to catching_up, and the
// resume read then sees the lost slot and reconciles the writes the gap
// missed. The route the integration guide gives is this one.
func TestCutoverRowNotSwappedWithALostSlotReconcilesOnceWrittenBack(t *testing.T) {
	f := newReconcileFixture(t, 300)
	shadow := f.buildShadow(t)
	c := newCopier(t, f, shadow)
	copyAll(t, f, c)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCutover, c.Position().Watermark, slot.Name(), slot.ConsistentPoint())))
	_, err = f.pool.Exec(t.Context(), `SELECT pg_catalog.pg_drop_replication_slot($1)`, slot.Name())
	require.NoError(t, err)
	f.exec(t, `UPDATE %s SET amount = 0 WHERE id BETWEEN 11 AND 20`)

	saved := f.load(t, shadow)
	_, err = schemachange.InspectResume(t.Context(), f.pool, f.target, saved)
	assert.Equal(t, schemachange.CauseResumeCutover, schemachange.RefusalCauseOf(err), "the refusal says nothing of the slot")
	_, err = schemachange.InspectSwapped(t.Context(), f.pool, f.lock, shadow.Proof(), schemachange.Options{})
	require.ErrorIs(t, err, schemachange.ErrNotSwapped)

	back := saved
	back.Phase = checkpoint.PhaseCatchingUp
	require.NoError(t, f.store.Save(t.Context(), f.lock, back))
	resume, err := schemachange.InspectResume(t.Context(), f.pool, f.target, f.load(t, shadow))
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeReconcile, resume.Mode)
	assert.False(t, resume.Found, "the slot is gone")

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), f.pool, f.load(t, shadow))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, reconciled.Slot.Close(ctx))
		assert.NoError(t, decode.DropSlot(ctx, f.cfg, f.pool, reconciled.Slot.Name()))
	})
	assert.Len(t, reconciled.Outcome.Repairs, 1, "the one chunk the update landed in was recopied")
	assert.Equal(t, checkpoint.PhaseCatchingUp, reconciled.Checkpoint.Phase)
}

// A row in any other phase whose tables have been replaced under their
// names — here by a swap the row does not record — is refused as
// relation-replaced before the marker is written: a recopy into the
// shadow's name would overwrite the live table. The row keeps its phase
// and no slot is made.
func TestReconcileRefusesReplacedRelationsBeforeItsMarker(t *testing.T) {
	f := newReconcileFixture(t, 200)
	shadow := f.buildShadow(t)
	copyAll(t, f, newCopier(t, f, shadow))
	cp := f.row(shadow, checkpoint.PhaseCatchingUp, copier.NewWatermark(math.MaxInt64), "", 0)
	require.NoError(t, f.store.Save(t.Context(), f.lock, cp))
	f.swapUnderTheNames(t, shadow)

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(t.Context(), f.pool, cp)
	assert.Equal(t, schemachange.CauseRelationReplaced, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)

	stored := f.load(t, shadow)
	assert.Equal(t, checkpoint.PhaseCatchingUp, stored.Phase, "the marker is written only once the relations are confirmed")
	assert.Empty(t, stored.SlotName)
	f.assertNoSlot(t)
}

// A reconcile that fails after making its slot leaves the row in
// reconciling and naming that slot — the marker goes first, and names the
// slot the reconcile owns — so the reaper reads the slot as a live row's,
// not an orphan, and the next resume reconciles again from the top.
func TestReconcileLeavesTheSlotItCreatedOwnedByTheRow(t *testing.T) {
	f := newReconcileFixture(t, 200)
	shadow := f.buildShadow(t)
	copyAll(t, f, newCopier(t, f, shadow))
	name := f.target.DecodingName()
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, name))
	})
	// The run saved its row before making a slot; the shadow has every row.
	cp := f.row(shadow, checkpoint.PhaseCatchingUp, copier.NewWatermark(math.MaxInt64), "", 0)
	require.NoError(t, f.store.Save(t.Context(), f.lock, cp))
	// The pass fails after the slot is made: the shadow has lost a column
	// the comparison reads.
	_, err := f.pool.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{f.table.Schema, shadow.ShadowTable()}.Sanitize()+` DROP COLUMN amount`)
	require.NoError(t, err)

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(t.Context(), f.pool, cp)
	require.Error(t, err)
	assert.False(t, errors.As(err, new(*schemachange.RefusalError)), "the pass failed; nothing was refused")

	stored := f.load(t, shadow)
	assert.Equal(t, checkpoint.PhaseReconciling, stored.Phase, "the marker outlives the failed attempt")
	assert.Equal(t, name, stored.SlotName, "the marker names the slot the reconcile owns")
	assert.Equal(t, name, stored.PublicationName)
	_, found, err := decode.InspectSlot(t.Context(), f.pool, name)
	require.NoError(t, err)
	assert.True(t, found, "the slot the attempt made stays for the next attempt to drop")

	reaped, err := f.store.ReapOrphanSlots(t.Context(), f.cfg)
	require.NoError(t, err)
	assert.Equal(t, []string{name}, reaped.Live, "the reaper reads the slot as the row's")
	assert.Empty(t, reaped.Dropped)

	resume, err := schemachange.InspectResume(t.Context(), f.pool, f.target, stored)
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeReconcile, resume.Mode)
	assert.True(t, resume.Found)
}

// A row whose watermark covers only part of the key space resumes in
// copying: the pass recopies only through the watermark, and the copier
// lands the rest under the new slot.
func TestReconcileResumesInCopyingBelowAPartialWatermark(t *testing.T) {
	f := newReconcileFixture(t, 300)
	shadow := f.buildShadow(t)
	copyAll(t, f, newCopier(t, f, shadow))
	// The row recorded the copier at key 150 when the slot was lost; the
	// gap then touched a key below and a key above it.
	cp := f.row(shadow, checkpoint.PhaseCopying, copier.NewWatermark(150), "", 0)
	require.NoError(t, f.store.Save(t.Context(), f.lock, cp))
	f.exec(t, `UPDATE %s SET amount = 0 WHERE id IN (100, 250)`)

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 50, MaxRows: 50},
	})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), f.pool, cp)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, reconciled.Slot.Close(ctx))
		assert.NoError(t, decode.DropSlot(ctx, f.cfg, f.pool, reconciled.Slot.Name()))
	})
	assert.Equal(t, checkpoint.PhaseCopying, reconciled.Checkpoint.Phase, "keys above the watermark are the copier's to land")
	assert.Equal(t, copier.NewWatermark(150), reconciled.Checkpoint.Watermark, "the watermark survived the reconcile")
	assertRepairsCoverExactly(t, reconciled.Outcome.Repairs, []int64{100}, []int64{250})
}

// A row that records another source statement than the built shadow's is
// refused, as a row for another target statement is: the pass would
// compare a shadow built for a different source shape.
func TestReconcileRefusesARowOfAnotherSourceShape(t *testing.T) {
	f := newReconcileFixture(t, 100)
	shadow := f.buildShadow(t)
	cp := f.row(shadow, checkpoint.PhaseCatchingUp, copier.NewWatermark(math.MaxInt64), f.target.DecodingName(), 0)
	cp.SourceFingerprint = "other"

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(t.Context(), f.pool, cp)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))
	f.assertNoSlot(t)
}

// Reconcile mode writes its marker through the checkpoint store, so a
// Reconciler without one is a programmer error, refused at construction
// rather than at the first save.
func TestNewReconcilerRequiresAStore(t *testing.T) {
	f := newReconcileFixture(t, 100)
	shadow := f.buildShadow(t)

	_, err := schemachange.NewReconciler(f.cfg, f.lock, nil, f.target, shadow, checksum.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.False(t, errors.As(err, new(*schemachange.RefusalError)), "a programmer error, not a catalog refusal")
}
