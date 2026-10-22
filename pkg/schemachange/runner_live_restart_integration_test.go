package schemachange_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests here drive RunOptions.Restart where the stopped run and the
// restart differ in kind, or the row names a slot the target does not
// derive: the restart discards what the row records, never what the
// current target would have made.

// quiescedTarget proves the fixture's table for a quiesced run on the
// same server: no logical decoding, so the run fences the source instead
// of streaming its changes.
func (f reconcileFixture) quiescedTarget(t *testing.T) preflight.CopySwapTarget {
	t.Helper()
	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.table.Schema, f.table.Table,
		preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	return target
}

// runOn carries the workload table through liveAlter on target with a
// fresh Runner over opts.
func (f reconcileFixture) runOn(t *testing.T, target preflight.CopySwapTarget, opts schemachange.RunOptions) (schemachange.Result, error) {
	t.Helper()
	r, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)
	return r.Run(t.Context(), f.pool, target, f.liveStatement(t))
}

// fenced reports whether the workload table carries the engine's write
// fence.
func (f reconcileFixture) fenced(t *testing.T) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_trigger tg
			JOIN pg_class c ON c.oid = tg.tgrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1
			  AND c.relname = $2
			  AND tg.tgname = $3
			  AND NOT tg.tgisinternal)`,
		f.table.Schema, f.table.Table, schemachange.WriteFenceTrigger).Scan(&exists))
	return exists
}

// deleteRow removes the table's row the way an operator would, under the
// table's lock, leaving whatever the stopped run made in place.
func (f reconcileFixture) deleteRow(t *testing.T) {
	t.Helper()
	row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.table.Schema, f.table.Table)
	require.NoError(t, err)
	require.NoError(t, f.store.Delete(t.Context(), lock, f.table.Schema, f.table.Table, row.Identity()))
	require.NoError(t, lock.Release(t.Context()))
}

// A shadow and a fence that no row records are discarded whichever kind
// of run the restart is: the fence of a quiesced leftover must not outlive
// a restart into a live run, whose source is meant to keep taking writes.
func TestRunnerRestartIntoALiveRunTakesAnUnrecordedFenceOff(t *testing.T) {
	f := newDecodingFixture(t, 100)
	_, err := f.runOn(t, f.quiescedTarget(t), schemachange.RunOptions{
		AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook,
	})
	require.ErrorIs(t, err, errStopHere)
	f.deleteRow(t)
	require.True(t, f.fenced(t), "the leftover fence stands with no row")

	var fencedDuringTheLiveRun bool
	hook := func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseCopying {
			fencedDuringTheLiveRun = f.fenced(t)
		}
		return errStopHere
	}
	_, err = f.runLive(t, schemachange.RunOptions{Restart: true, AfterCheckpoint: hook})
	require.ErrorIs(t, err, errStopHere)
	assert.False(t, fencedDuringTheLiveRun, "the live run's source must take writes")
}

// The hold a restart releases is read from the row, not from the current
// target: a live run's slot pins WAL until it is dropped, so a quiesced
// restart over a stopped live run drops that slot before it runs fresh.
func TestRunnerQuiescedRestartOverALiveRowDropsItsSlot(t *testing.T) {
	f := newDecodingFixture(t, 100)
	_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook})
	require.ErrorIs(t, err, errStopHere)
	row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	require.NotEmpty(t, row.SlotName)

	result, err := f.runOn(t, f.quiescedTarget(t), schemachange.RunOptions{Restart: true})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	_, found, err := decode.InspectSlot(t.Context(), f.pool, row.SlotName)
	require.NoError(t, err)
	assert.False(t, found, "the stopped live run's slot is dropped")
}

// A restart drops only the slot the target derives, the same check a
// resume makes: a row naming another engine slot is not this target's
// run, so the restart refuses as resume-row-mismatch before it discards
// anything, and the slot, the shadow, and the row are left as found.
func TestRunnerRestartRefusesARowNamingAnotherEngineSlot(t *testing.T) {
	f := newDecodingFixture(t, 100)
	_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook})
	require.ErrorIs(t, err, errStopHere)
	row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	require.Equal(t, f.target.DecodingName(), row.SlotName)

	// Another well-formed engine slot name, one hex digit away from the
	// target's, so it passes DropSlot's own engine-name guard.
	other := row.SlotName[:len(row.SlotName)-1] + "0"
	if other == row.SlotName {
		other = row.SlotName[:len(row.SlotName)-1] + "1"
	}
	require.True(t, decode.IsEngineSlotName(other))
	_, err = f.pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, other)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, err := f.pool.Exec(ctx, `
			SELECT pg_drop_replication_slot(slot_name)
			FROM pg_replication_slots
			WHERE slot_name = ANY($1)`, []string{other, row.SlotName})
		assert.NoError(t, err)
	})
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.table.Schema, f.table.Table)
	require.NoError(t, err)
	edited := row
	edited.SlotName = other
	require.NoError(t, f.store.Save(t.Context(), lock, edited))
	require.NoError(t, lock.Release(t.Context()))

	_, err = f.runLive(t, schemachange.RunOptions{Restart: true})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))
	_, found, err := decode.InspectSlot(t.Context(), f.pool, other)
	require.NoError(t, err)
	assert.True(t, found, "the slot the row names is not the restart's to drop")
	after, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	assert.Equal(t, other, after.SlotName, "the row is left as found")
	var shadowStands bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`,
		pgx.Identifier{f.table.Schema, row.ShadowTable}.Sanitize()).Scan(&shadowStands))
	assert.True(t, shadowStands, "the refusal comes before the shadow is discarded")
}
