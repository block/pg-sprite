package schemachange_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests here drive the Runner under RunOptions.Restart: the operator's
// way of starting a stopped change over instead of resuming it. The
// stopped run's shadow, hold, and row are discarded inside the restart's
// own lock session and the change runs fresh; the one row a restart will
// not discard is a cutover row whose swap has already committed.

// restartAlter is the statement every restart here carries.
const restartAlter = `ALTER TABLE %s.orders DROP COLUMN note`

// committedCutoverRow leaves orders as a run does when it stops between
// the swap's commit and the row's move to done: the swap committed and
// the row still in cutover.
func (f runnerFixture) committedCutoverRow(t *testing.T) schemachange.BuiltShadow {
	t.Helper()
	lock := f.lock(t, "orders")
	s := f.stageUnder(t, lock, "orders", restartAlter)
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
		Statement:         f.alter(t, restartAlter).SQL(),
	}))
	_, err = f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	require.NoError(t, lock.Release(t.Context()))
	return s.built
}

// A run that stopped in verifying on a shadow whose rows no longer match
// the source cannot be resumed: every resume reads the whole key space
// again and finds the same difference. A restart discards that shadow
// and its fence along with the row and runs the change from the start:
// the new shadow is built from the source, so the pass finds no
// difference, the swap runs, and the table has the statement's shape.
func TestRunnerRestartDiscardsAStoppedRunAndStartsOver(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	sourceOID := f.relationOID(t, "orders")
	stop := &stopAfter{phase: checkpoint.PhaseVerifying}

	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	stopped := f.inspectRow(t, "orders")
	shadow := pgx.Identifier{f.schema, stopped.ShadowTable}.Sanitize()
	f.exec(t, "UPDATE "+shadow+" SET qty = 0 WHERE id = 5")
	stoppedShadowOID := f.relationOID(t, stopped.ShadowTable)

	_, err = f.run(t, "orders", restartAlter, schemachange.RunOptions{})
	var divergence *checksum.DivergenceError
	require.ErrorAs(t, err, &divergence, "a resume meets the difference every time")
	require.True(t, f.hasFence(t, "orders"), "the stopped run's fence stands")

	restarted := &stopAfter{}
	result, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true, KeepOld: true, AfterCheckpoint: restarted.hook})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	// The two copying saves are a fresh run's: the row recorded as the
	// restart's shadow is built, then the finished copy recording its
	// complete watermark before the verifying save.
	assert.Equal(t, []checkpoint.Phase{checkpoint.PhaseCopying, checkpoint.PhaseCopying, checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone},
		restarted.seen(), "the restart ran every phase from the start")
	assert.NotEqual(t, stoppedShadowOID, result.Shadow.ShadowOID, "the restart built a shadow of its own")
	assert.Equal(t, sourceOID, result.Swapped.OldOID, "the source the stopped run fenced is the one swapped out")
	assert.False(t, f.hasColumn(t, "orders", "note"))
	assert.False(t, f.hasFence(t, "orders"), "the live table takes writes")
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: schemachange.OldName(f.schema, "orders")},
		testutil.RelationRef{Schema: f.schema, Table: "orders"},
		testutil.ConvergeOptions{IgnoreColumns: []string{"note"}})
}

// A restart on a table with nothing to discard — no row, no shadow, no
// fence — is a plain run: the flag asks to discard what a stopped run
// left, and finding nothing is not an error.
func TestRunnerRestartWithNothingToDiscardRunsPlain(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	sourceOID := f.relationOID(t, "orders")

	result, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, sourceOID, result.Swapped.OldOID)
	assert.False(t, f.hasColumn(t, "orders", "note"))
}

// A relation under the shadow's name that no row records is refused by a
// plain run as shadow-unrecorded, since nothing says what built it. A
// restart is the operator saying it is leftover: the relation is dropped
// and the change runs fresh, building a shadow of its own under the same
// name.
func TestRunnerRestartDropsAShadowNoRowRecords(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	f.exec(t, `CREATE TABLE %s.`+shadow+` (id bigint PRIMARY KEY)`)
	leftoverOID := f.relationOID(t, shadow)

	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{})
	require.Equal(t, schemachange.CauseShadowUnrecorded, schemachange.RefusalCauseOf(err), "a plain run refuses the leftover")

	result, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.NotEqual(t, leftoverOID, result.Shadow.ShadowOID, "the leftover was dropped, not adopted")
	assert.False(t, f.hasColumn(t, "orders", "note"))
}

// A restart met with a cutover row whose swap committed is refused as
// restart-swap-committed and discards nothing: the shadow already bears
// the source's name, so there is no earlier state to go back to. The row
// stays in cutover, and a run without the restart finishes the change
// from the catalog, as any resume past the swap does.
func TestRunnerRestartRefusesARowWhoseSwapCommitted(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	sourceOID := f.relationOID(t, "orders")
	built := f.committedCutoverRow(t)

	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseRestartSwapCommitted, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase, "the refusal writes nothing")
	assert.Equal(t, built.ShadowOID(), f.relationOID(t, "orders"), "the swapped-in shadow is still live")
	assert.True(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the retained source is still there")

	result, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{})
	require.NoError(t, err, "a run without the restart finishes the change")
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, sourceOID, result.Swapped.OldOID)
	assert.True(t, result.OldTableDropped)
	assert.False(t, f.hasColumn(t, "orders", "note"))
}

// A cutover row whose swap never ran — the run stopped between the
// cutover save and the swap — is a run before its point of no return,
// and a restart discards it like any other stopped run.
func TestRunnerRestartDiscardsACutoverRowWhoseSwapNeverRan(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	sourceOID := f.relationOID(t, "orders")
	stop := &stopAfter{phase: checkpoint.PhaseCutover}

	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	require.Equal(t, sourceOID, f.relationOID(t, "orders"), "the swap never ran")
	stoppedShadowOID := f.relationOID(t, f.inspectRow(t, "orders").ShadowTable)

	result, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.NotEqual(t, stoppedShadowOID, result.Shadow.ShadowOID, "the restart built a shadow of its own")
	assert.Equal(t, sourceOID, result.Swapped.OldOID)
	assert.False(t, f.hasColumn(t, "orders", "note"))
}

// A restart that stopped after dropping a cutover row's shadow, before it
// deleted the row, is retried by the same restart: the swap never ran, so
// the catalog still shows the source under its own name and no shadow,
// the missing shadow counts as already discarded, and the change runs
// fresh.
func TestRunnerRestartRetriedAfterItDroppedACutoverRowsShadow(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	sourceOID := f.relationOID(t, "orders")
	stop := &stopAfter{phase: checkpoint.PhaseCutover}
	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	stopped := f.inspectRow(t, "orders")
	f.exec(t, `DROP TABLE %s.`+stopped.ShadowTable)

	result, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, sourceOID, result.Swapped.OldOID)
	assert.False(t, f.hasFence(t, "orders"), "the stopped run's fence is off")
	assert.False(t, f.hasColumn(t, "orders", "note"))
}

// A cutover row whose swap committed and whose retained source was
// already dropped is past the point of starting over just the same: the
// shadow bears the source's name, so a restart refuses and leaves the row
// and the live table as found.
func TestRunnerRestartRefusesACommittedSwapWhoseRetainedSourceIsGone(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	built := f.committedCutoverRow(t)
	f.exec(t, `DROP TABLE %s.`+schemachange.OldName(f.schema, "orders"))

	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseRestartSwapCommitted, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase)
	assert.Equal(t, built.ShadowOID(), f.relationOID(t, "orders"), "the swapped-in table is untouched")
}

// A cutover row whose retained source was renamed away from its derived
// name is not a swap the catalog answers cleanly: the restart refuses
// with the catalog's own cause, discards nothing, and leaves the row for
// an operator.
func TestRunnerRestartRefusesACutoverRowTheCatalogDoesNotAnswerCleanly(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	built := f.committedCutoverRow(t)
	f.exec(t, `ALTER TABLE %s.`+schemachange.OldName(f.schema, "orders")+` RENAME TO orders_kept`)

	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseRelationReplaced, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase, "the row is left as found")
	assert.Equal(t, built.ShadowOID(), f.relationOID(t, "orders"))
	assert.True(t, f.relationExists(t, "orders_kept"))
}

// The restart releases the hold only after the shadow, and deletes the
// row last, so a restart that stops partway leaves a row that still
// records what stands. Here the fence's drop cannot get its lock while a
// reader holds the table: the shadow is already gone, and the row and the
// fence both still stand.
func TestRunnerRestartThatStopsAtTheFenceLeavesTheRow(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	stop := &stopAfter{phase: checkpoint.PhaseVerifying}
	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	stopped := f.inspectRow(t, "orders")
	require.True(t, f.hasFence(t, "orders"))

	release := f.holdAccessShare(t, "orders")
	_, err = f.run(t, "orders", restartAlter, schemachange.RunOptions{
		Restart: true,
		Shadow:  schemachange.Options{LockTimeout: 300 * time.Millisecond},
	})
	require.Error(t, err, "the fence cannot be dropped while a reader holds the table")
	release()

	assert.Equal(t, stopped.ShadowTable, f.inspectRow(t, "orders").ShadowTable, "the row still records the run")
	assert.True(t, f.hasFence(t, "orders"), "the fence still stands, and the row says so")
	assert.False(t, f.relationExists(t, stopped.ShadowTable), "the shadow was discarded first")
}

// A done row is the completed change's record, and its retained source
// is the rollback window. A restart does not discard it, whatever
// ReplaceDone says about the row.
func TestRunnerRestartDoesNotDiscardADoneRowOrItsRollbackWindow(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{KeepOld: true})
	require.NoError(t, err)
	old := schemachange.OldName(f.schema, "orders")
	require.True(t, f.relationExists(t, old))

	for _, replaceDone := range []bool{false, true} {
		_, err = f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true, ReplaceDone: replaceDone})
		require.Error(t, err)
		assert.Equal(t, schemachange.CauseResumeTerminal, schemachange.RefusalCauseOf(err), "ReplaceDone=%v", replaceDone)
		assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t, "orders").Phase)
		assert.True(t, f.relationExists(t, old), "the rollback window stands")
	}
}
