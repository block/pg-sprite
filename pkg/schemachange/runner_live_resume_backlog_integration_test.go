package schemachange_test

import (
	"fmt"
	"testing"
	"time"

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
// first resume: below the copy's WAL, so a copy that stopped before its
// stream confirmed any of it leaves the slot over the ceiling.
const backlogCeiling = 128 << 20

// raisedBacklogCeiling is the ceiling the second resume runs under: above
// the WAL the slot holds unconfirmed, with room for the resume's own copy.
const raisedBacklogCeiling = 512 << 20

// backlogRunOptions is liveRunOptions under ceiling, with a short catch-up
// cycle so the first reading of the slot comes well before the stream
// could read through the backlog.
func backlogRunOptions(ceiling int64, opts schemachange.RunOptions) schemachange.RunOptions {
	opts.Catchup = applier.CatchupOptions{SlotLagCeiling: ceiling, Interval: 10 * time.Millisecond}
	return opts
}

// A run stopped in its copy before the stream confirmed any of the copy's
// WAL leaves the slot holding more unconfirmed WAL than the ceiling. A
// resume under the same ceiling reads the slot on its first catch-up cycle,
// before the stream has read through that backlog, and stops stream-behind
// again; a resume under a ceiling above the backlog drains it and carries
// the table to done. That is the remedy the stream-behind stop names.
//
// The stopped copy is the shadow holding every source row with no
// watermark saved, as a process killed before its first watermark save
// leaves it: the rows are written into the shadow after the first save by
// the same insert-from-select the copy runs. The shadow is not published,
// so the stream reads past that WAL with nothing to apply.
func TestRunnerResumesAStreamBehindBacklogUnderAHigherCeiling(t *testing.T) {
	f := newDecodingFixture(t, backlogRows)
	_, err := f.runLive(t, backlogRunOptions(backlogCeiling, schemachange.RunOptions{
		AfterCheckpoint: (&stopAfter{phase: checkpoint.PhaseCopying}).hook,
	}))
	require.ErrorIs(t, err, errStopHere)
	row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	require.False(t, row.Watermark.Valid(), "the stopped run saved no watermark")
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s
		SELECT *
		FROM %s`,
		pgx.Identifier{f.table.Schema, row.ShadowTable}.Sanitize(), f.table.Qualified()))
	require.NoError(t, err)
	stopped, found, err := decode.InspectSlot(t.Context(), f.pool, row.SlotName)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, stopped.Unconfirmed.Known)
	require.Greater(t, stopped.Unconfirmed.Bytes, int64(backlogCeiling), "the stopped copy left the slot over the ceiling")

	_, err = f.runLive(t, backlogRunOptions(backlogCeiling, schemachange.RunOptions{}))
	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, decode.SlotLagStreamBehind, exceeded.Cause, "the first reading comes before the stream has read through the backlog")
	assert.Greater(t, exceeded.Unconfirmed, int64(backlogCeiling))

	result, err := f.runLive(t, backlogRunOptions(raisedBacklogCeiling, schemachange.RunOptions{}))
	require.NoError(t, err)
	f.assertLiveDone(t, result)
}
