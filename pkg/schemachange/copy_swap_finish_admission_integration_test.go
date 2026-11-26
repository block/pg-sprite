package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// otherAlter is a second statement on the orders table createOrders makes,
// for a run that follows one which finished restartAlter.
const otherAlter = `ALTER TABLE %s.orders ALTER COLUMN id TYPE bigint`

// finishOnlyTarget leaves orders with a committed cutover row and returns
// the target the documented composition mints for it on a volume with a
// byte free: one admitted only to finish that swap.
func (f runnerFixture) finishOnlyTarget(t *testing.T) preflight.CopySwapTarget {
	t.Helper()
	f.committedCutoverRow(t)
	env := preflight.CopySwapEnvironment{FreeDiskBytes: tightVolume}
	target, err := schemachange.CheckCopySwap(t.Context(), f.pool, f.store, f.schema, "orders", env, schemachange.CopySwapResume)
	require.NoError(t, err)
	require.True(t, target.FinishesCommittedSwap(), "the tight volume admits only finishing the swap")
	return target
}

// finishWithRoom is a second caller that checks the environment with room
// to spare and finishes the stopped run first.
func (f runnerFixture) finishWithRoom(t *testing.T) {
	t.Helper()
	roomy := preflight.CopySwapEnvironment{FreeDiskBytes: 1 << 40}
	target, err := schemachange.CheckCopySwap(t.Context(), f.pool, f.store, f.schema, "orders", roomy, schemachange.CopySwapResume)
	require.NoError(t, err)
	result, err := f.runner(t, schemachange.RunOptions{ReplaceDone: true}).Run(t.Context(), f.pool, target, f.alter(t, restartAlter))
	require.NoError(t, err)
	require.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
}

// A target admitted on one free byte to finish a committed swap is not
// good for a fresh run. Another caller finishes the stopped run between
// the check and this Run, so the row is done, and ReplaceDone would
// replace it and copy the whole table on a volume measured for none of
// it. The run refuses before it writes: the done row stays and no shadow
// is built.
func TestRunRefusesAFinishOnlyTargetOnceTheRowIsDone(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	stale := f.finishOnlyTarget(t)
	f.finishWithRoom(t)

	_, err := f.runner(t, schemachange.RunOptions{ReplaceDone: true}).Run(t.Context(), f.pool, stale, f.alter(t, otherAlter))
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseFinishAdmissionStale, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t, "orders").Phase, "the done row is not replaced")
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "no shadow is built")
}

// The same target is not good for resuming a row whose swap has not
// committed either. After the stopped run is finished, a run of another
// statement stops at its cutover save, before its swap: resuming that
// row verifies and swaps again over its shadow, which the finish-only
// admission never measured. The run refuses and the row stays in cutover
// with its shadow.
//
// The second statement is
//
//	ALTER TABLE orders ALTER COLUMN id TYPE bigint
func TestRunRefusesAFinishOnlyTargetOnARowWhoseSwapDidNotCommit(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	stale := f.finishOnlyTarget(t)
	f.finishWithRoom(t)
	stop := &stopAfter{phase: checkpoint.PhaseCutover}
	_, err := f.run(t, "orders", otherAlter, schemachange.RunOptions{ReplaceDone: true, AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)

	_, err = f.runner(t, schemachange.RunOptions{}).Run(t.Context(), f.pool, stale, f.alter(t, otherAlter))
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseFinishAdmissionStale, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase, "the refusal wrote nothing")
	assert.True(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "the shadow stays for a run checked again")
}

// An operator who drops the retained source by hand to free space, then
// runs again, leaves a committed swap with nothing to drop. The read still
// proves the commit — the retained source's OID is gone from the catalog —
// so the tight volume admits the run, and it reaches done.
func TestCheckCopySwapAdmitsACommittedSwapWhoseRetainedSourceIsAlreadyDropped(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	f.committedCutoverRow(t)
	f.exec(t, `DROP TABLE %s.`+schemachange.OldName(f.schema, "orders"))

	env := preflight.CopySwapEnvironment{FreeDiskBytes: tightVolume}
	target, err := schemachange.CheckCopySwap(t.Context(), f.pool, f.store, f.schema, "orders", env, schemachange.CopySwapResume)
	require.NoError(t, err)
	assert.True(t, target.FinishesCommittedSwap())
	result, err := f.runner(t, schemachange.RunOptions{}).Run(t.Context(), f.pool, target, f.alter(t, restartAlter))
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.True(t, result.OldTableDropped)
}
