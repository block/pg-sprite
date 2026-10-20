package schemachange_test

import (
	"encoding/json"
	"math"
	"testing"

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

	_, err = f.run(t, "orders", restartAlter, schemachange.RunOptions{Restart: true})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseRestartSwapCommitted, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase, "the refusal writes nothing")
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the swapped-in shadow is still live")
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
