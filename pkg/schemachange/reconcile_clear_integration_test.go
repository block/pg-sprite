package schemachange_test

import (
	"context"
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

// A row whose watermark covers part of the key space can sit over a shadow
// that holds rows above it: chunks the earlier run committed but never
// checkpointed. The pass compares only through the watermark, so such a
// row is outside every comparison and yet holds its values. When the
// source has since moved a unique value out of one of those rows into a
// chunk the pass finds differing, the recopy would collide with the row
// above on every attempt, with no comparison able to clear it. The
// reconcile clears the shadow above the watermark before its pass, as the
// copier does before it resumes, so the recopy lands first time; the row
// resumes in copying at the same watermark, and the copier that resumes
// from it lands the rest.
func TestReconcileClearsTheShadowAboveAPartialWatermarkBeforeItsRecopy(t *testing.T) {
	f := newReconcileFixture(t, 500)
	shadow := f.buildShadow(t)
	copyAll(t, f, newCopier(t, f, shadow))
	// The run checkpointed the copier at key 300 when its slot was lost;
	// the shadow holds keys 301 through 500 from chunks the row never
	// recorded.
	cp := f.row(shadow, checkpoint.PhaseCopying, copier.NewWatermark(300), "", 0)
	require.NoError(t, f.store.Save(t.Context(), f.lock, cp))

	// Nothing captures the source while the slot is gone: a chunk below
	// the watermark changes, and the unique value of key 400 — a shadow
	// row above the watermark — moves to key 160, in that differing
	// chunk.
	f.exec(t, `UPDATE %s SET amount = 0 WHERE id = 150`)
	f.exec(t, `UPDATE %s SET uniq = -400 WHERE id = 400`)
	f.exec(t, `UPDATE %s SET uniq = 400 WHERE id = 160`)

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), f.pool, cp)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, reconciled.Slot.Close(ctx))
		assert.NoError(t, decode.DropSlot(ctx, f.cfg, f.pool, reconciled.Slot.Name()))
	})

	assert.Equal(t, 0, reconciled.Outcome.Collisions, "the shadow row holding the moved value was cleared before the recopy")
	assertRepairsCoverExactly(t, reconciled.Outcome.Repairs, []int64{150, 160}, []int64{50, 250})
	assert.Equal(t, checkpoint.PhaseCopying, reconciled.Checkpoint.Phase, "keys above the watermark are the copier's to land")
	assert.Equal(t, copier.NewWatermark(300), reconciled.Checkpoint.Watermark, "the watermark survived the reconcile")
	assert.Equal(t, 0, f.shadowRowsAbove(t, shadow, 300), "the reconcile cleared every shadow row above the watermark")

	// The copier resumes from the reconciled row's watermark and the
	// shadow converges.
	resumed, err := copier.NewCopier(f.target, shadow, f.lock, reconciled.Checkpoint.Watermark, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	copyAll(t, f, resumed)
	f.assertConverged(t, shadow)
}

// shadowRowsAbove counts the shadow rows whose key lies above key.
func (f reconcileFixture) shadowRowsAbove(t *testing.T, shadow schemachange.BuiltShadow, key int64) int {
	t.Helper()
	table := pgx.Identifier{f.table.Schema, shadow.ShadowTable()}.Sanitize()
	var n int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE id > $1`, key).Scan(&n))
	return n
}
