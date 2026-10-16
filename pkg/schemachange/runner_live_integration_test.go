package schemachange_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// The tests here drive the Runner over a target proven to decode WAL, with
// the workload table written the whole time: the application does not
// pause for a live run. Row-by-row comparison of the retained source and
// the live table after the swap is the oracle, so a change the stream or
// the drain missed shows as a difference.

// liveAlter is the statement every live run here carries: the workload
// table's amount column widens, which the copy, the stream, and the drain
// all carry without converting a value.
const liveAlter = `ALTER TABLE %s ALTER COLUMN amount TYPE numeric(14,2)`

// liveStatement parses liveAlter against the fixture's table.
func (f reconcileFixture) liveStatement(t *testing.T) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne(fmt.Sprintf(liveAlter, f.table.Qualified()))
	require.NoError(t, err)
	return st
}

// liveWriterInterval paces the test writer: fast enough that every phase
// up to the live pass runs under writes, slow enough that the catch-up's
// marker wait is a wait and not a chase.
const liveWriterInterval = 20 * time.Millisecond

// liveRunOptions sizes every phase to thousand-row chunks and keeps the
// old table, so the test can compare it with the live one, with opts'
// other fields laid over.
func liveRunOptions(opts schemachange.RunOptions) schemachange.RunOptions {
	opts.Copier.Workers = 2
	opts.Copier.Chunker = gateChunks
	opts.Checksum.Chunker = gateChunks
	opts.KeepOld = true
	return opts
}

// runLive carries the workload table through liveAlter with a fresh
// Runner over opts.
func (f reconcileFixture) runLive(t *testing.T, opts schemachange.RunOptions) (schemachange.Result, error) {
	t.Helper()
	r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(opts))
	require.NoError(t, err)
	return r.Run(t.Context(), f.pool, f.target, f.liveStatement(t))
}

// untilVerifying returns an AfterCheckpoint hook that stops w at the
// verifying save and then calls next, recording the writer's count in
// writes. The writer runs through the copy and the catch-up and stops as
// the live pass begins: the changes it made that the stream has yet to
// deliver are the pass's to settle, while a chunk written faster than the
// pass can read it twice is the unresolved-chunk retry's case, not this
// one's; and a write landing after the swap committed belongs to the live
// table alone, which the old-versus-live oracle would count as a
// difference rather than as the behaviour it is.
func untilVerifying(t *testing.T, w *sourceWriter, writes *int, next func(context.Context, checkpoint.Checkpoint) error) func(context.Context, checkpoint.Checkpoint) error {
	return func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseVerifying && *writes == 0 {
			*writes = w.stop(t)
		}
		if next == nil {
			return nil
		}
		return next(ctx, cp)
	}
}

// amountType is the declared type of the live table's amount column.
func (f reconcileFixture) amountType(t *testing.T) string {
	t.Helper()
	var typ string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = 'amount'`,
		f.table.Schema, f.table.Table).Scan(&typ))
	return typ
}

// assertLiveDone checks a live run's outcome: the row is done and still
// names the slot it ran on, the slot itself is gone, the live table has
// the statement's shape, and the retained source and the live table hold
// the same rows.
func (f reconcileFixture) assertLiveDone(t *testing.T, result schemachange.Result) {
	t.Helper()
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, f.target.DecodingName(), result.Checkpoint.SlotName, "the done row records the slot the run decoded from")
	_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found, "the slot is dropped once the swap committed")
	assert.Equal(t, "numeric(14,2)", f.amountType(t), "the gated statement's shape is live")
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.table.Schema, Table: schemachange.OldName(f.table.Schema, f.table.Table)},
		testutil.RelationRef{Schema: f.table.Schema, Table: f.table.Table},
		testutil.ConvergeOptions{})
}

// One Run carries a table that is being written from no row to done. The
// writer updates a row in every chunk and inserts a new one on each tick,
// throughout the copy and the catch-up, and stops at the verifying save
// with its last changes still in the stream for the live pass to settle;
// the AfterCheckpoint hook then writes three more changes once the row
// says cutover — after the capture has stopped, so only the swap's drain
// can carry them. The live table holds every one of
// them: the writer's rows, the drained insert, the drained update, and
// not the drained delete's row.
func TestRunnerCarriesALiveTableFromNoRowToDone(t *testing.T) {
	const seeded = 2500
	f := newDecodingFixture(t, seeded)
	w := f.startWriter(t, seeded, liveWriterInterval)

	drained := func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCutover {
			return nil
		}
		for _, sql := range []string{
			`INSERT INTO %s (uniq, amount, label, blob) VALUES (900001, 9, 'drained', 'x')`,
			`UPDATE %s SET amount = 777 WHERE id = 1`,
			`DELETE FROM %s WHERE id = 2`,
		} {
			if _, err := f.pool.Exec(ctx, fmt.Sprintf(sql, f.table.Qualified())); err != nil {
				return err
			}
		}
		return nil
	}
	var writes int
	result, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: untilVerifying(t, w, &writes, drained)})
	require.NoError(t, err)

	f.assertLiveDone(t, result)
	assert.Positive(t, writes, "the source was written while the run was in progress")
	assert.Positive(t, f.liveRows(t), "the writer's inserts are in the live table")
	assert.True(t, result.Verification.Clean(), "the live pass settled every chunk the stream was still writing")
	var amount string
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT amount::text FROM %s WHERE id = 1`, f.table.Qualified())).Scan(&amount))
	assert.Equal(t, "777.00", amount, "the update made after capture stopped was drained under the swap's lock")
	var n int
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = 2 OR label = 'drained'`, f.table.Qualified())).Scan(&n))
	assert.Equal(t, 1, n, "the drained insert is live and the drained delete's row is gone")
}

// A live run stopped right after any of its saves resumes with a fresh
// Runner to done, under the same writer, with the shadow and the slot the
// first run left: the resumed run reopens the stream at the row's
// position, so every write between the stop and the resume is carried.
// A stop after the cutover save, before the swap ran, is the case where
// the catalog answers not swapped and the resume catches up again.
func TestRunnerResumesALiveRunFromEverySave(t *testing.T) {
	for _, phase := range []checkpoint.Phase{
		checkpoint.PhaseCopying,
		checkpoint.PhaseCatchingUp,
		checkpoint.PhaseVerifying,
		checkpoint.PhaseCutover,
	} {
		t.Run(phase.String(), func(t *testing.T) {
			const seeded = 2500
			f := newDecodingFixture(t, seeded)
			w := f.startWriter(t, seeded, liveWriterInterval)
			stop := &stopAfter{phase: phase}

			var first int
			_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: untilVerifying(t, w, &first, stop.hook)})
			require.ErrorIs(t, err, errStopHere)
			if first == 0 {
				// A stop before the verifying save left the writer running.
				first = w.stop(t)
			}
			require.Positive(t, first, "the source was written while the first run was in progress")
			row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
			require.NoError(t, err)
			require.Equal(t, phase, row.Phase)
			require.Equal(t, f.target.DecodingName(), row.SlotName)
			status, found, err := decode.InspectSlot(t.Context(), f.pool, row.SlotName)
			require.NoError(t, err)
			require.True(t, found, "a stopped run leaves its slot for the resume")
			require.False(t, status.Active, "the stopped run's stream let the slot go")
			before := f.liveRows(t)

			// The application writes on between the stop and the resume.
			w = f.startWriterFrom(t, seeded, liveWriterInterval, liveUniqBase+first+1)
			second := &stopAfter{}
			var writes int
			result, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: untilVerifying(t, w, &writes, second.hook)})
			require.NoError(t, err)

			f.assertLiveDone(t, result)
			assert.Greater(t, f.liveRows(t), before, "the source was written between the stop and the resume")
			assert.Positive(t, writes)
			assert.Equal(t, row.ShadowTable, result.Shadow.ShadowTable, "the resumed run continued on the shadow the first built")
			assert.False(t, result.Reconciled, "an intact slot is resumed, not reconciled")
		})
	}
}

// A live run whose slot is gone when it resumes — dropped from under the
// row while nothing held it — reconciles (ST-4): the slot is replaced,
// every landed chunk is compared and recopied where it differs, and the
// run goes on to done under the same writer.
func TestRunnerResumesALiveRunWhoseSlotWasDropped(t *testing.T) {
	const seeded = 2500
	f := newDecodingFixture(t, seeded)
	w := f.startWriter(t, seeded, liveWriterInterval)
	stop := &stopAfter{phase: checkpoint.PhaseCatchingUp}

	_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	require.NoError(t, decode.DropSlot(t.Context(), f.cfg, f.pool, f.target.DecodingName()))
	// The writes the lost slot never carried are the reconcile's to recopy.
	first := w.stop(t)
	require.Positive(t, first)
	w = f.startWriterFrom(t, seeded, liveWriterInterval, liveUniqBase+first+1)

	second := &stopAfter{}
	var writes int
	result, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: untilVerifying(t, w, &writes, second.hook)})
	require.NoError(t, err)

	f.assertLiveDone(t, result)
	assert.Positive(t, writes)
	assert.True(t, result.Reconciled, "the lost slot was reconciled before the stream reopened")
	assert.Equal(t, checkpoint.PhaseCatchingUp, second.seen()[0], "the reconcile hands the run a row catching up on the new slot")
}

// A row a quiesced run wrote is refused by a live run, and a row a live
// run wrote is refused by a quiesced run, both as resume-row-decoding:
// the two never continue each other's rows (ST-3). Nothing of the stopped
// run is touched by the refusal.
func TestRunnerRefusesTheOtherKindOfRunsRow(t *testing.T) {
	t.Run("quiesced row under a live target", func(t *testing.T) {
		f := newDecodingFixture(t, 100)
		quiesced, err := preflight.CheckCopySwap(t.Context(), f.pool, f.table.Schema, f.table.Table,
			preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
		require.NoError(t, err)
		require.False(t, quiesced.DecodesWAL())
		stop := &stopAfter{phase: checkpoint.PhaseCopying}
		r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{AfterCheckpoint: stop.hook}))
		require.NoError(t, err)
		_, err = r.Run(t.Context(), f.pool, quiesced, f.liveStatement(t))
		require.ErrorIs(t, err, errStopHere)

		_, err = f.runLive(t, schemachange.RunOptions{})
		require.Error(t, err)
		assert.Equal(t, schemachange.CauseResumeRowDecoding, schemachange.RefusalCauseOf(err))
		_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
		require.NoError(t, err)
		assert.False(t, found, "no slot was made for a row that names none")
	})
	t.Run("live row under a quiesced target", func(t *testing.T) {
		f := newDecodingFixture(t, 100)
		stop := &stopAfter{phase: checkpoint.PhaseCopying}
		_, err := f.runLive(t, schemachange.RunOptions{AfterCheckpoint: stop.hook})
		require.ErrorIs(t, err, errStopHere)

		quiesced, err := preflight.CheckCopySwap(t.Context(), f.pool, f.table.Schema, f.table.Table,
			preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
		require.NoError(t, err)
		r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{}))
		require.NoError(t, err)
		_, err = r.Run(t.Context(), f.pool, quiesced, f.liveStatement(t))
		require.Error(t, err)
		assert.Equal(t, schemachange.CauseResumeRowDecoding, schemachange.RefusalCauseOf(err))
		_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
		require.NoError(t, err)
		assert.True(t, found, "the live run's slot is left for the live run to resume")
	})
}
