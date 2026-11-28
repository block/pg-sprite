package schemachange_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// backlogRows seeds a table whose copy writes about 200 MiB of WAL: each
// row carries a ten-KiB value stored out of line, which the copy writes to
// the shadow's TOAST table again.
const backlogRows = 20000

// backlogCeiling is the slot lag ceiling of the stopped run and of the
// refused resume: below the copy's WAL, so a copy that stopped before its
// stream confirmed any of it leaves the slot over the ceiling.
const backlogCeiling = 128 << 20

// raisedBacklogCeiling is the ceiling the admitted resume runs under:
// above the WAL the slot holds unconfirmed, with room for the resume's own
// copy.
const raisedBacklogCeiling = 512 << 20

// backlogRunOptions is liveRunOptions under ceiling.
func backlogRunOptions(ceiling int64, opts schemachange.RunOptions) schemachange.RunOptions {
	opts.Catchup = applier.CatchupOptions{SlotLagCeiling: ceiling}
	return opts
}

// A run stopped in its copy before the stream confirmed any of the copy's
// WAL leaves the slot holding more unconfirmed WAL than the ceiling. A
// resume under the same ceiling is refused as resume-slot-backlog before
// it opens the stream or restarts the copy, so it writes nothing: the
// shadow keeps the rows it had, the slot keeps its confirmed and restart
// positions, and the checkpoint row is as the stop left it. A resume under
// a ceiling above the backlog is admitted, drains it, and carries the
// table to done.
//
// The stopped copy is the shadow holding every source row with no
// watermark saved, as a process killed before its first watermark save
// leaves it: the rows are written into the shadow after the first save by
// the same insert-from-select the copy runs. The shadow is not published,
// so the stream reads past that WAL with nothing to apply.
func TestRunnerRefusesAResumeOverItsSlotBacklogUntilTheCeilingIsRaised(t *testing.T) {
	f := newDecodingFixture(t, backlogRows)
	_, err := f.runLive(t, backlogRunOptions(backlogCeiling, schemachange.RunOptions{
		AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook,
	}))
	require.ErrorIs(t, err, errStopHere)
	row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	require.False(t, row.Watermark.Valid(), "the stopped run saved no watermark")
	shadow := pgx.Identifier{f.table.Schema, row.ShadowTable}.Sanitize()
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s
		SELECT *
		FROM %s`,
		shadow, f.table.Qualified()))
	require.NoError(t, err)
	stopped, found, err := decode.InspectSlot(t.Context(), f.pool, row.SlotName)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, stopped.Unconfirmed.Known)
	require.Greater(t, stopped.Unconfirmed.Bytes, int64(backlogCeiling), "the stopped copy left the slot over the ceiling")

	_, err = f.runLive(t, backlogRunOptions(backlogCeiling, schemachange.RunOptions{}))
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeSlotBacklog, schemachange.RefusalCauseOf(err))
	assert.NotErrorIs(t, err, decode.ErrSlotLagCeiling, "refused before any catch-up cycle read the slot")
	var shadowRows int
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s`, shadow)).Scan(&shadowRows))
	assert.Equal(t, backlogRows, shadowRows, "the refused resume cleared nothing from the shadow")
	refused, found, err := decode.InspectSlot(t.Context(), f.pool, row.SlotName)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, stopped.ConfirmedFlushLSN, refused.ConfirmedFlushLSN, "no stream confirmed anything")
	assert.Equal(t, stopped.RestartLSN, refused.RestartLSN)
	after, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	assert.Equal(t, row, after, "the refused resume saved nothing")

	result, err := f.runLive(t, backlogRunOptions(raisedBacklogCeiling, schemachange.RunOptions{}))
	require.NoError(t, err)
	f.assertLiveDone(t, result)
}
