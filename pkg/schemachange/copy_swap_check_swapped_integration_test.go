package schemachange_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// tightVolume is the free space the tests below report: one byte, far
// below what any copy of the orders table needs, so only a run that
// grows nothing on the volume is admitted.
const tightVolume int64 = 1

// A run that died after its swap committed, before the done save, left a
// cutover row, the shadow under the source's name, and the retained
// source under its _old name, still on the volume. Finishing it drops the
// retained source and saves done, which frees space and builds nothing,
// so the documented composition — schemachange.CheckCopySwap, then
// Runner.Run on the target it mints — admits it on a volume with a byte
// free, and the run reaches done. The fresh-run preflight, which knows
// nothing of the stopped run, is refused at the same figure: it asks for
// twice the table.
//
// The statement is
//
//	ALTER TABLE orders DROP COLUMN note
//
// on the orders table createOrders makes.
func TestCheckCopySwapAdmitsFinishingACommittedSwapOnATightVolume(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	sourceOID := f.relationOID(t, "orders")

	// The run up to and including the swap, by hand, with the row saved
	// the way the Runner saves it before calling Cutover: a crash right
	// after the swap's commit leaves exactly this.
	lock := f.lock(t, "orders")
	s := f.stageUnder(t, lock, "orders", alter)
	proof, err := json.Marshal(s.built)
	require.NoError(t, err)
	require.NoError(t, f.store.Ensure(t.Context()))
	require.NoError(t, f.store.Save(t.Context(), lock, checkpoint.Checkpoint{
		Schema:            f.schema,
		Table:             "orders",
		ShadowTable:       s.built.ShadowTable(),
		Watermark:         copier.NewWatermark(math.MaxInt64),
		SourceFingerprint: s.built.SourceFingerprint(),
		TargetFingerprint: s.built.TargetFingerprint(),
		Phase:             checkpoint.PhaseCutover,
		ShadowProof:       proof,
		Statement:         f.alter(t, alter).SQL(),
	}))
	_, err = f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	require.NoError(t, lock.Release(t.Context()))
	require.True(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the retained source is still on the volume")

	env := preflight.CopySwapEnvironment{FreeDiskBytes: tightVolume}
	_, err = preflight.CheckCopySwap(t.Context(), f.pool, f.schema, "orders", env)
	envErr, ok := errors.AsType[*preflight.CopySwapEnvironmentError](err)
	require.True(t, ok, "a fresh run on the tight volume is a disk refusal: %v", err)
	assert.Equal(t, preflight.CopySwapCauseDiskHeadroom, envErr.Cause)

	target, err := schemachange.CheckCopySwap(t.Context(), f.pool, f.store, f.schema, "orders", env, schemachange.CopySwapResume)
	require.NoError(t, err, "finishing a committed swap is asked for no disk")

	result, err := f.runner(t, schemachange.RunOptions{}).Run(t.Context(), f.pool, target, f.alter(t, alter))
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t, "orders").Phase)
	assert.Zero(t, result.Verification, "a resume past the swap copies and verifies nothing")
	assert.True(t, result.FinishedEarlierSwap, "the run only finished the earlier run's swap")
	assert.True(t, result.SwapProven, "the swap was re-derived from the catalog")
	assert.Equal(t, s.built.ShadowOID(), result.Swapped.LiveOID)
	assert.Equal(t, sourceOID, result.Swapped.OldOID)
	assert.True(t, result.OldTableDropped)
	assert.False(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the retained source was dropped, freeing its space")
	assert.False(t, f.hasColumn(t, "orders", "note"))
}

// A run stopped right after its cutover save, before the swap ran, also
// left a cutover row, but its swap did not commit: the source is still
// live and the resume verifies and gates again before it swaps, over the
// shadow it built. The preflight measures that row as any resume that
// still copies: it discounts the shadow it finds under the shadow's name
// and asks for the rest of the copy's headroom, so the tight volume that
// admits finishing a committed swap refuses it.
func TestCheckCopySwapStillAsksACutoverRowWhoseSwapDidNotCommitForTheCopy(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	sourceOID := f.relationOID(t, "orders")
	stop := &stopAfter{phase: checkpoint.PhaseCutover}

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	require.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase)
	require.Equal(t, sourceOID, f.relationOID(t, "orders"), "the swap never ran")
	shadow := schemachange.ShadowName(f.schema, "orders")
	require.True(t, f.relationExists(t, shadow), "the shadow still bears its own name")

	env := preflight.CopySwapEnvironment{FreeDiskBytes: tightVolume}
	_, err = schemachange.CheckCopySwap(t.Context(), f.pool, f.store, f.schema, "orders", env, schemachange.CopySwapResume)
	envErr, ok := errors.AsType[*preflight.CopySwapEnvironmentError](err)
	require.True(t, ok, "a resume that still copies is a disk refusal on the tight volume: %v", err)
	assert.Equal(t, preflight.CopySwapCauseDiskHeadroom, envErr.Cause)
	assert.Equal(t, tightVolume, envErr.FreeDiskBytes)
	assert.Positive(t, envErr.HeldShadowBytes, "the shadow built so far is discounted")
	assert.Less(t, envErr.HeldShadowBytes, envErr.RequiredShadowBytes, "only the shadow is discounted, not the whole headroom")
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase, "the refusal wrote nothing")
}
