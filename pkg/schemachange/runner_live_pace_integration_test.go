package schemachange_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests here drive a live run's copy pace through a whole Run. Whether
// a real stream's lag crosses the pace mark depends on the timing of the
// run, so the lag the copy reads is put over the mark on cue; everything
// else — the copier, the capture, the swap — is the run's own.

// pacedRows seeds the table with three of liveRunOptions' thousand-row
// chunks, so there is a chunk left to claim after the first.
const pacedRows = 2500

// A live run's copy waits before a chunk while the stream's lag reads over
// its pace mark, goes on once the lag reads within it, and the Result says
// how far the copy was held. The lag reads over the mark for its first
// three readings: the first worker to read it waits at least one poll, and
// no wait runs out, since the readings return within the mark long before
// the copier's minute-long wait could.
func TestLiveRunReportsHowFarItsStreamLagHeldTheCopy(t *testing.T) {
	f := newDecodingFixture(t, pacedRows)
	const poll = 10 * time.Millisecond
	r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{
		Live: checksum.LiveOptions{Poll: poll},
	}))
	require.NoError(t, err)
	schemachange.PaceOverMark(r, 3, nil)

	result, err := r.Run(t.Context(), f.pool, f.target, f.liveStatement(t))
	require.NoError(t, err)

	f.assertLiveDone(t, result)
	assert.Positive(t, result.CopyPaced.Waits, "the copy waited while the lag read over the mark")
	assert.Zero(t, result.CopyPaced.Expired, "every wait ended with the lag back within the mark")
	assert.GreaterOrEqual(t, result.CopyPaced.Waited, poll, "a held worker waits a poll before it reads the lag again")
}

// A live run cancelled while its copy is held by the stream's lag ends as
// the cancellation it is, before any worker claims a chunk: a held worker
// has nothing in flight, so the row stays copying with no watermark and
// the shadow holds no row. The lag reads over the mark at every reading,
// and the run is cancelled at the first, while that worker is held.
func TestLiveRunCancelledWhileItsCopyIsHeldClaimsNoChunk(t *testing.T) {
	f := newDecodingFixture(t, pacedRows)
	r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{}))
	require.NoError(t, err)
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	schemachange.PaceOverMark(r, math.MaxInt64, cancel)

	_, err = r.Run(runCtx, f.pool, f.target, f.liveStatement(t))
	require.ErrorIs(t, err, context.Canceled)

	row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseCopying, row.Phase, "the cancelled run left its row copying")
	assert.Equal(t, copier.Watermark{}, row.Watermark, "no chunk landed")
	var copied int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM `+pgx.Identifier{f.table.Schema, row.ShadowTable}.Sanitize()).Scan(&copied))
	assert.Zero(t, copied, "no worker claimed a chunk while it was held")
}
