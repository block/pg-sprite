package schemachange_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests here drive Runner.Discard: the caller's way of ending a
// copy-and-swap run without running the change. Whatever the stopped run
// left — its shadow, its hold on the source, its checkpoint row — is
// dropped under the table's lock, and every outcome is checked against the
// catalog itself: pg_class for the shadow, pg_replication_slots and
// pg_publication for a live run's hold, pg_trigger for a quiesced run's
// fence, and the checkpoint table for the row.

// proveShape proves table's privileges and shape for copy-and-swap, the
// proof a discard takes: no environment check, since a discard grows
// nothing.
func proveShape(t *testing.T, pool *pgxpool.Pool, schema, table string, logicalDecoding bool) preflight.CopySwapShape {
	t.Helper()
	role, err := preflight.CheckPrivileges(t.Context(), pool, schema, table,
		preflight.Requirement{Tier: preflight.TierCopyAndSwap, LogicalDecoding: logicalDecoding})
	require.NoError(t, err)
	shape, err := preflight.CheckCopySwapShape(t.Context(), pool, schema, table, role)
	require.NoError(t, err)
	return shape
}

// discardWith discards shape's table with a fresh Runner over store.
func discardWith(t *testing.T, cfg dbconn.Config, store *checkpoint.Store, pool *pgxpool.Pool, shape preflight.CopySwapShape) (schemachange.Discarded, error) {
	t.Helper()
	r, err := schemachange.NewRunner(cfg, store, schemachange.RunOptions{})
	require.NoError(t, err)
	return r.Discard(t.Context(), pool, shape)
}

// leftoverState is what the catalog shows under the names a run on one
// table makes.
type leftoverState struct {
	Shadow, Slot, Publication, Fence, Row bool
}

// readLeftovers reads leftoverState for shape's table straight from the
// catalog and the checkpoint table.
func readLeftovers(t *testing.T, pool *pgxpool.Pool, shape preflight.CopySwapShape) leftoverState {
	t.Helper()
	var s leftoverState
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1
			  AND c.relname = $2
		)`, shape.Schema(), schemachange.ShadowName(shape.Schema(), shape.Table())).Scan(&s.Shadow))
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_replication_slots
			WHERE slot_name = $1
		)`, shape.DecodingName()).Scan(&s.Slot))
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_publication
			WHERE pubname = $1
		)`, shape.DecodingName()).Scan(&s.Publication))
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_trigger tg
			JOIN pg_class c ON c.oid = tg.tgrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1
			  AND c.relname = $2
			  AND tg.tgname = $3
		)`, shape.Schema(), shape.Table(), schemachange.WriteFenceTrigger).Scan(&s.Fence))
	checkpointTable := pgx.Identifier{checkpoint.SchemaName, checkpoint.TableName}.Sanitize()
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM `+checkpointTable+`
			WHERE schema_name = $1
			  AND table_name = $2
		)`, shape.Schema(), shape.Table()).Scan(&s.Row))
	return s
}

// A live run cancelled while it copies leaves its shadow, its slot and
// publication, and its row. Discard drops all four and reports each by
// name with the phase the row was in; the change is not run, so the
// source keeps its shape. A fresh run afterwards starts from the
// beginning: its first save has nothing landed and it builds a shadow of
// its own.
func TestRunnerDiscardDropsALiveRunCancelledMidCopy(t *testing.T) {
	f := newDecodingFixture(t, 5000)
	shape := proveShape(t, f.pool, f.table.Schema, f.table.Table, true)

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelOnceLanded := func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseCopying && cp.Watermark.Valid() {
			cancel()
		}
		return nil
	}
	r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{
		AfterCheckpoint:    cancelOnceLanded,
		CheckpointInterval: time.Millisecond,
	}))
	require.NoError(t, err)
	_, err = r.Run(runCtx, f.pool, f.target, f.liveStatement(t))
	require.ErrorIs(t, err, context.Canceled)
	stopped, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	require.True(t, stopped.Watermark.Valid(), "the cancelled run had landed rows")
	require.False(t, stopped.Phase.Terminal())
	require.Equal(t, leftoverState{Shadow: true, Slot: true, Publication: true, Row: true}, readLeftovers(t, f.pool, shape),
		"the cancelled run leaves its shadow, slot, publication, and row")

	discarded, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.NoError(t, err)
	assert.Equal(t, schemachange.Discarded{
		Schema:      f.table.Schema,
		Table:       f.table.Table,
		RowPhase:    stopped.Phase,
		Shadow:      stopped.ShadowTable,
		Slot:        stopped.SlotName,
		Publication: stopped.PublicationName,
	}, discarded)
	assert.False(t, discarded.Nothing())
	assert.Equal(t, leftoverState{}, readLeftovers(t, f.pool, shape), "nothing of the run remains")
	assert.Equal(t, "numeric(12,2)", f.amountType(t), "the discard does not run the change")

	var first checkpoint.Checkpoint
	recordFirst := func(_ context.Context, cp checkpoint.Checkpoint) error {
		if first.Phase == 0 {
			first = cp
		}
		return nil
	}
	result, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: recordFirst})
	require.NoError(t, err)
	f.assertLiveDone(t, result)
	assert.Equal(t, checkpoint.PhaseCopying, first.Phase)
	assert.False(t, first.Watermark.Valid(), "the fresh run starts its copy from the beginning")
}

// A discard that completed leaves nothing, so a second one — an
// orchestrator calling again after a crash that lost the first answer —
// finds nothing to discard and says so without an error. The first
// discards a quiesced run stopped in verifying: its shadow, the write
// fence on the source, and its row; the source takes writes again.
func TestRunnerDiscardCalledTwiceFindsNothingTheSecondTime(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shape := proveShape(t, f.pool, f.schema, "orders", false)
	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseVerifying}).hook})
	require.ErrorIs(t, err, errStopHere)
	require.Equal(t, leftoverState{Shadow: true, Fence: true, Row: true}, readLeftovers(t, f.pool, shape))

	discarded, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.NoError(t, err)
	assert.Equal(t, schemachange.Discarded{
		Schema:     f.schema,
		Table:      "orders",
		RowPhase:   checkpoint.PhaseVerifying,
		Shadow:     schemachange.ShadowName(f.schema, "orders"),
		WriteFence: true,
	}, discarded)
	assert.Equal(t, leftoverState{}, readLeftovers(t, f.pool, shape))
	f.assertWritable(t, "orders")
	assert.True(t, f.hasColumn(t, "orders", "note"), "the discard does not run the change")

	again, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.NoError(t, err, "nothing to discard is an answer, not an error")
	assert.True(t, again.Nothing())
	assert.Equal(t, schemachange.Discarded{Schema: f.schema, Table: "orders"}, again)
}

// A run in progress holds the table's lock for as long as it runs, so a
// discard is refused at once with the lock-held error naming the run's
// session, never waits for it, and touches nothing: the run's shadow,
// fence, and row stand. Once the run has stopped and let the lock go, the
// same discard goes through.
func TestRunnerDiscardRefusesWhileARunHoldsTheTableLock(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shape := proveShape(t, f.pool, f.schema, "orders", false)

	entered := make(chan struct{})
	release := make(chan struct{})
	holdAtFirstSave := func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCopying || cp.Watermark.Valid() {
			return nil
		}
		close(entered)
		<-release
		return errStopHere
	}
	runErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{AfterCheckpoint: holdAtFirstSave})
		runErr <- err
	})
	t.Cleanup(wg.Wait)
	var releaseOnce sync.Once
	stopRun := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(stopRun)

	const holdDeadline = 30 * time.Second
	select {
	case <-entered:
	case <-time.After(holdDeadline):
		t.Fatalf("the run did not reach its first save within %s", holdDeadline)
	}

	_, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	var held *dbconn.TableLockHeldError
	require.ErrorAs(t, err, &held)
	assert.Equal(t, f.schema, held.Schema)
	assert.Equal(t, "orders", held.Table)
	assert.NotZero(t, held.Holder.PID, "the refusal names the run's session")
	assert.Equal(t, leftoverState{Shadow: true, Fence: true, Row: true}, readLeftovers(t, f.pool, shape),
		"the refused discard dropped nothing")

	stopRun()
	require.ErrorIs(t, <-runErr, errStopHere)
	discarded, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseCopying, discarded.RowPhase)
	assert.Equal(t, leftoverState{}, readLeftovers(t, f.pool, shape))
}

// A cutover row whose swap committed is past the point of discarding: the
// shadow is the live table under the source's name and the source is the
// retained table under its _old name. The discard is refused as
// restart-swap-committed, as a restart is, and drops nothing; a run of the
// same statement then finishes the change.
func TestRunnerDiscardRefusesARowWhoseSwapCommitted(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	built := f.committedCutoverRow(t)
	old := schemachange.OldName(f.schema, "orders")
	shape := proveShape(t, f.pool, f.schema, "orders", false)

	_, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseRestartSwapCommitted, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase, "the row is left as found")
	assert.Equal(t, built.ShadowOID(), f.relationOID(t, "orders"), "the swapped-in table is still live")
	assert.True(t, f.relationExists(t, old), "the retained source is still there")

	result, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{})
	require.NoError(t, err, "a run of the statement finishes the change")
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.False(t, f.relationExists(t, old))
}

// A shadow, a slot, and a publication that no checkpoint row records —
// a live run stopped after its first save whose row an operator then
// deleted — are dropped by a discard with no row to read: the shadow as a
// restart drops one, the slot and publication as the orphan reaper drops
// them. The report names each and no row phase.
func TestRunnerDiscardDropsAnUnrecordedShadowAndSlot(t *testing.T) {
	f := newDecodingFixture(t, 100)
	shape := proveShape(t, f.pool, f.table.Schema, f.table.Table, true)
	_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook})
	require.ErrorIs(t, err, errStopHere)
	f.deleteRow(t)
	require.Equal(t, leftoverState{Shadow: true, Slot: true, Publication: true}, readLeftovers(t, f.pool, shape),
		"the shadow, slot, and publication stand with no row")

	discarded, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.NoError(t, err)
	assert.Equal(t, schemachange.Discarded{
		Schema:      f.table.Schema,
		Table:       f.table.Table,
		Shadow:      schemachange.ShadowName(f.table.Schema, f.table.Table),
		Slot:        shape.DecodingName(),
		Publication: shape.DecodingName(),
	}, discarded)
	assert.Equal(t, leftoverState{}, readLeftovers(t, f.pool, shape))
}
