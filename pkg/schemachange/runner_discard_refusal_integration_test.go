package schemachange_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// The tests here drive the refusals of Runner.Discard: what it leaves
// standing because it is not the stopped run's to drop, or because the
// caller's proof or a holder stops it, and what it reports it dropped
// before a refusal or a failure.

// holdSlot opens a stream on the fixture's slot and waits until its
// walsender holds the slot; the caller closes the stream.
func holdSlot(t *testing.T, f reconcileFixture) *decode.Stream {
	t.Helper()
	stream, err := decode.OpenStream(t.Context(), f.cfg, f.pool, f.target, f.currentWALLSN(t))
	require.NoError(t, err)
	const activeDeadline = 10 * time.Second
	const activePoll = 20 * time.Millisecond
	require.Eventually(t, func() bool {
		status, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
		require.NoError(t, err)
		return found && status.Active
	}, activeDeadline, activePoll, "the stream's walsender should hold the slot")
	return stream
}

// A done row records a change that completed: a discard refuses it as
// resume-row-terminal and drops nothing, so the retained source of a
// KeepOld run stays for the caller's rollback window.
func TestRunnerDiscardRefusesADoneRow(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shape := proveShape(t, f.pool, f.schema, "orders", false)
	result, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{KeepOld: true})
	require.NoError(t, err)
	require.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	old := schemachange.OldName(f.schema, "orders")

	discarded, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeTerminal, schemachange.RefusalCauseOf(err))
	assert.True(t, discarded.Nothing(), "the refused discard dropped nothing")
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t, "orders").Phase, "the done row is left as found")
	assert.True(t, f.relationExists(t, old), "the retained source is left")
}

// A nil pool is a wiring mistake in the caller: the discard returns an
// invariant violation before it takes the table's lock, as Run does,
// rather than panic.
func TestRunnerDiscardWithANilPoolIsAnInvariantViolation(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shape := proveShape(t, f.pool, f.schema, "orders", false)
	r, err := schemachange.NewRunner(f.cfg, f.store, schemachange.RunOptions{})
	require.NoError(t, err)

	var discarded schemachange.Discarded
	require.NotPanics(t, func() { discarded, err = r.Discard(t.Context(), nil, shape) })
	require.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.Discarded{}, discarded)
}

// Two tables of one database can derive the same slot name, since the
// name carries a short hash; the test searches for such a pair. Table
// b has a stopped live run whose row names the slot. Table a has no row,
// so a discard of a reads every table's rows for the slot under the slot's
// lock, finds b's, and refuses the slot as resume-slot-foreign: b's slot,
// publication, and row stand for b's run to resume.
func TestRunnerDiscardKeepsTheSlotAnotherTablesRowNames(t *testing.T) {
	f := newDecodingFixture(t, 10)
	schema := f.table.Schema
	var db string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&db))
	nameA, nameB := collidingTables(db, schema)
	for _, name := range []string{nameA, nameB} {
		table := pgx.Identifier{schema, name}.Sanitize()
		_, err := f.pool.Exec(t.Context(), `
			CREATE TABLE `+table+` (
				id bigint PRIMARY KEY,
				label text
			)`)
		require.NoError(t, err)
		_, err = f.pool.Exec(t.Context(), `
			INSERT INTO `+table+` (id, label)
			SELECT g, 'x'
			FROM generate_series(1, 20) g`)
		require.NoError(t, err)
	}

	targetB, err := preflight.CheckCopySwap(t.Context(), f.pool, schema, nameB,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	st, err := statement.ParseOne(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN label`, pgx.Identifier{schema, nameB}.Sanitize()))
	require.NoError(t, err)
	r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{
		AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook,
	}))
	require.NoError(t, err)
	_, err = r.Run(t.Context(), f.pool, targetB, st)
	require.ErrorIs(t, err, errStopHere)
	rowB, err := f.store.Inspect(t.Context(), schema, nameB)
	require.NoError(t, err)
	require.Equal(t, targetB.DecodingName(), rowB.SlotName)

	shapeA := proveShape(t, f.pool, schema, nameA, true)
	require.Equal(t, rowB.SlotName, shapeA.DecodingName(), "the two tables derive one slot name")
	discarded, err := discardWith(t, f.cfg, f.store, f.pool, shapeA)
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeSlotForeign, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, decode.ErrForeignDecodingState)
	assert.Equal(t, schemachange.Discarded{Schema: schema, Table: nameA}, discarded, "table a had nothing of its own to drop")
	assert.Equal(t, leftoverState{Slot: true, Publication: true}, readLeftovers(t, f.pool, shapeA),
		"table b's slot and publication stand")
	stillB, err := f.store.Inspect(t.Context(), schema, nameB)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseCopying, stillB.Phase)
	assert.Equal(t, rowB.SlotName, stillB.SlotName, "table b's row still names its slot")
}

// collidingTables returns two table names of schema whose derived slot
// names in database db are the same, found by trying names in order.
func collidingTables(db, schema string) (string, string) {
	seen := map[string]string{}
	for i := 0; ; i++ {
		name := fmt.Sprintf("c%d", i)
		slot := preflight.CopySwapDecodingName(db, schema, name)
		if prior, ok := seen[slot]; ok {
			return prior, name
		}
		seen[slot] = name
	}
}

// A slot of the table's derived name that is physical is not the engine's:
// a discard with no row neither counts it as a leftover nor drops it, and
// finds nothing to discard.
func TestRunnerDiscardLeavesAPhysicalSlotOfTheDerivedName(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shape := proveShape(t, f.pool, f.schema, "orders", false)
	_, err := f.pool.Exec(t.Context(), `SELECT pg_create_physical_replication_slot($1)`, shape.DecodingName())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `SELECT pg_drop_replication_slot($1)`, shape.DecodingName())
		assert.NoError(t, err)
	})

	discarded, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.NoError(t, err)
	assert.True(t, discarded.Nothing())
	assert.Equal(t, leftoverState{Slot: true}, readLeftovers(t, f.pool, shape), "the physical slot stands")
}

// A slot is dropped over a replication connection, so a shape whose proof
// did not verify replication access is refused before anything is
// dropped, whether a row records the slot or not: the shadow, the slot,
// the publication, and the row all stand.
func TestRunnerDiscardRefusesASlotWithoutAReplicationProofBeforeDroppingAnything(t *testing.T) {
	f := newDecodingFixture(t, 100)
	shape := proveShape(t, f.pool, f.table.Schema, f.table.Table, false)
	_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook})
	require.ErrorIs(t, err, errStopHere)

	discarded, err := discardWith(t, f.cfg, f.store, f.pool, shape)
	require.ErrorIs(t, err, preflight.ErrCopySwapProofMismatch)
	assert.True(t, discarded.Nothing(), "the row path dropped nothing")
	assert.Equal(t, leftoverState{Shadow: true, Slot: true, Publication: true, Row: true}, readLeftovers(t, f.pool, shape))

	f.deleteRow(t)
	discarded, err = discardWith(t, f.cfg, f.store, f.pool, shape)
	require.ErrorIs(t, err, preflight.ErrCopySwapProofMismatch)
	assert.True(t, discarded.Nothing(), "the no-row path dropped nothing")
	assert.Equal(t, leftoverState{Shadow: true, Slot: true, Publication: true}, readLeftovers(t, f.pool, shape))
}

// With no row, a slot a walsender holds is refused at once with
// decode.ErrSlotActive, never waited for: the shadow is dropped first and
// reported beside the error, and the slot and publication stand.
func TestRunnerDiscardRefusesAHeldUnrecordedSlotAfterDroppingTheShadow(t *testing.T) {
	f := newDecodingFixture(t, 100)
	shape := proveShape(t, f.pool, f.table.Schema, f.table.Table, true)
	_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook})
	require.ErrorIs(t, err, errStopHere)
	f.deleteRow(t)
	stream := holdSlot(t, f)
	t.Cleanup(func() { assert.NoError(t, stream.Close(context.WithoutCancel(t.Context()))) })

	// The deadline only bounds a discard that waits for the holder, which
	// this one must not do.
	const discardDeadline = 30 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), discardDeadline)
	defer cancel()
	r, err := schemachange.NewRunner(f.cfg, f.store, schemachange.RunOptions{})
	require.NoError(t, err)
	discarded, err := r.Discard(ctx, f.pool, shape)
	require.ErrorIs(t, err, decode.ErrSlotActive)
	assert.Equal(t, schemachange.Discarded{
		Schema: f.table.Schema,
		Table:  f.table.Table,
		Shadow: schemachange.ShadowName(f.table.Schema, f.table.Table),
	}, discarded, "the shadow went before the held slot was refused")
	assert.Equal(t, leftoverState{Slot: true, Publication: true}, readLeftovers(t, f.pool, shape))
}

// A row's slot is dropped with the waiting drop, bounded by the caller's
// context. With a walsender holding it, a discard under a short deadline
// drops the shadow, then waits at the slot until the deadline, and reports
// the shadow beside the deadline error; the slot, publication, and row
// stand. Once the holder lets go, a second discard drops the rest and
// reports the row's phase with the slot and publication.
func TestRunnerDiscardOfARowWhoseSlotIsHeldStopsAfterTheShadow(t *testing.T) {
	f := newDecodingFixture(t, 100)
	shape := proveShape(t, f.pool, f.table.Schema, f.table.Table, true)
	_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook})
	require.ErrorIs(t, err, errStopHere)
	stream := holdSlot(t, f)

	const discardDeadline = 4 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), discardDeadline)
	defer cancel()
	r, err := schemachange.NewRunner(f.cfg, f.store, schemachange.RunOptions{})
	require.NoError(t, err)
	discarded, err := r.Discard(ctx, f.pool, shape)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, schemachange.Discarded{
		Schema: f.table.Schema,
		Table:  f.table.Table,
		Shadow: schemachange.ShadowName(f.table.Schema, f.table.Table),
	}, discarded, "the shadow went before the wait at the slot")
	assert.Equal(t, leftoverState{Slot: true, Publication: true, Row: true}, readLeftovers(t, f.pool, shape))

	require.NoError(t, stream.Close(t.Context()))
	discarded, err = discardWith(t, f.cfg, f.store, f.pool, shape)
	require.NoError(t, err)
	assert.Equal(t, schemachange.Discarded{
		Schema:      f.table.Schema,
		Table:       f.table.Table,
		RowPhase:    checkpoint.PhaseCopying,
		Slot:        shape.DecodingName(),
		Publication: shape.DecodingName(),
	}, discarded)
	assert.Equal(t, leftoverState{}, readLeftovers(t, f.pool, shape))
}
