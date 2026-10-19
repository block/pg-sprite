package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A second change on a table whose row is done runs under ReplaceDone
// without the caller clearing the row first: the run deletes the done row
// under its own lock and starts fresh, and the table ends with the second
// statement's shape under a done row that records the second swap.
func TestRunnerReplacesADoneRowUnderItsOwnLock(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	first, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{})
	require.NoError(t, err)
	require.Equal(t, checkpoint.PhaseDone, first.Checkpoint.Phase)

	second, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN sku`, schemachange.RunOptions{ReplaceDone: true})
	require.NoError(t, err)

	assert.Equal(t, checkpoint.PhaseDone, second.Checkpoint.Phase)
	assert.Equal(t, first.Shadow.ShadowOID, second.Swapped.OldOID, "the second run started from the table the first swapped in")
	assert.Equal(t, second.Shadow.ShadowOID, f.relationOID(t, "orders"), "the live table is the second run's shadow")
	assert.NotEqual(t, first.Checkpoint.TargetFingerprint, second.Checkpoint.TargetFingerprint, "the row records the second statement's shape")
	assert.False(t, f.hasColumn(t, "orders", "sku"))
	assert.False(t, f.hasColumn(t, "orders", "note"), "the first change stays applied")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
	assert.True(t, second.OldTableDropped)
	row := f.inspectRow(t, "orders")
	assert.Equal(t, checkpoint.PhaseDone, row.Phase)
	assert.Equal(t, second.Checkpoint.TargetFingerprint, row.TargetFingerprint, "the done row is the second run's")
}

// A done row whose retained source still stands — the first run kept its
// old table under KeepOld for a rollback window — is not replaced: the
// row's proof is the only record of which relation that is, so the run
// is refused as resume-row-terminal and the window stays open. Once the
// caller ends the window with DropOldTable, the same run replaces the row.
func TestRunnerKeepsADoneRowWhoseRetainedSourceStands(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	first, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{KeepOld: true})
	require.NoError(t, err)
	old := schemachange.OldName(f.schema, "orders")
	require.True(t, f.relationExists(t, old))
	before := f.inspectRow(t, "orders")

	_, err = f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN sku`, schemachange.RunOptions{ReplaceDone: true})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeTerminal, schemachange.RefusalCauseOf(err))
	assert.Equal(t, before, f.inspectRow(t, "orders"), "the refused run leaves the done row as it found it")
	assert.True(t, f.relationExists(t, old), "the rollback window stays open")
	assert.True(t, f.hasColumn(t, "orders", "sku"), "nothing was built for the second statement")

	lock := f.lock(t, "orders")
	swapped, err := schemachange.InspectSwapped(t.Context(), f.pool, lock, first.Shadow, schemachange.Options{})
	require.NoError(t, err)
	require.NoError(t, schemachange.DropOldTable(t.Context(), f.pool, lock, swapped, schemachange.Options{}))
	require.NoError(t, lock.Release(t.Context()))

	second, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN sku`, schemachange.RunOptions{ReplaceDone: true})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, second.Checkpoint.Phase)
	assert.False(t, f.hasColumn(t, "orders", "sku"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
}

// A failed row is never replaced, ReplaceDone or not: nothing this Runner
// writes puts a row in PhaseFailed, so the row is another engine's to
// clear, and the run is refused as resume-row-terminal with the row and
// the shadow it names left in place.
func TestRunnerNeverReplacesAFailedRow(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	built := f.leaveStoppedRun(t, `ALTER TABLE %s.orders DROP COLUMN note`, func(cp *checkpoint.Checkpoint) {
		cp.Phase = checkpoint.PhaseFailed
	})
	before := f.inspectRow(t, "orders")

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN sku`, schemachange.RunOptions{ReplaceDone: true})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeTerminal, schemachange.RefusalCauseOf(err))
	assert.Equal(t, before, f.inspectRow(t, "orders"), "the refused run leaves the failed row as it found it")
	assert.True(t, f.relationExists(t, built.ShadowTable()), "the shadow the failed run built stays for the operator")
	assert.True(t, f.hasColumn(t, "orders", "sku"))
}

// ReplaceDone replaces only a done row. A row in copying records a run
// still under way, and a run under ReplaceDone resumes it on the shadow
// it built, as a run without the option does.
func TestRunnerResumesAnUnfinishedRowUnderReplaceDone(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	stop := &stopAfter{phase: checkpoint.PhaseCopying}
	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	row := f.inspectRow(t, "orders")
	require.Equal(t, checkpoint.PhaseCopying, row.Phase)
	shadowOID := f.relationOID(t, row.ShadowTable)

	result, err := f.run(t, "orders", alter, schemachange.RunOptions{ReplaceDone: true})
	require.NoError(t, err)

	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, shadowOID, result.Shadow.ShadowOID, "the run resumed on the shadow the stopped run built")
	assert.Equal(t, shadowOID, result.Swapped.LiveOID)
	assert.False(t, f.hasColumn(t, "orders", "note"))
}
