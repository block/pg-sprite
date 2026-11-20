package schemachange_test

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// pacePoll is the live poll of the run here: while the copy runs, the
// pace's wait is the one sleep of this length the run takes, so a Sleep
// that sees it is inside a wait that has already been counted.
const pacePoll = 11 * time.Millisecond

// paceStopCeiling is the run's slot lag ceiling. The copy is paced at a
// quarter of it, 4 MiB.
const paceStopCeiling = 16 << 20

// walFillerTable takes the WAL the test writes to move the run's lag. It
// is not the run's table, so the stream carries none of its rows, but
// every byte of WAL it takes counts in the slot's unconfirmed WAL.
const walFillerTable = `
	CREATE TABLE public.wal_filler (
		id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		body text NOT NULL
	)`

// fillWAL commits rows of a thousand characters into the filler table,
// each a little over a KiB of WAL: too short for TOAST to compress.
func fillWAL(ctx context.Context, pool *pgxpool.Pool, rows int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO public.wal_filler (body)
		SELECT repeat('x', 1000)
		FROM generate_series(1, $1) AS g`, rows)
	return err
}

// A live run the lag ceiling stops as stream-behind says on the error how
// far its copy was held back before the stop, since Run returns no Result
// with an error. The copy reads the stream's lag over its pace mark at
// every reading, so it waits before its first chunk however the run is
// timed. The capture is held from confirming past one change the source
// never makes visible, written at the run's first save, so every later
// byte of WAL counts as the stream's lag. Once that hold is in place, the
// copy's first wait writes about 26 MiB, which the next catch-up cycle
// reads over the 16 MiB ceiling.
func TestRunnerStoppedStreamBehindReportsHowFarItsCopyWasHeldBack(t *testing.T) {
	const seeded = 2500
	f := newDecodingFixtureOn(t, seeded, synchronousStandbyThatNeverConnects)
	hidden := newHiddenCommit(t, f)
	_, err := f.pool.Exec(t.Context(), walFillerTable)
	require.NoError(t, err)

	holdConfirmation := hidden.whenIn(t, checkpoint.PhaseCopying)
	var held atomic.Bool
	var overCeiling sync.Once
	opts := liveRunOptions(schemachange.RunOptions{
		Catchup: applier.CatchupOptions{SlotLagCeiling: paceStopCeiling, Interval: 10 * time.Millisecond},
		Live:    checksum.LiveOptions{Poll: pacePoll},
		AfterCheckpoint: func(ctx context.Context, cp checkpoint.Checkpoint) error {
			if err := holdConfirmation(ctx, cp); err != nil {
				return err
			}
			if cp.Phase == checkpoint.PhaseCopying {
				held.Store(true)
			}
			return nil
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			if d == pacePoll && held.Load() {
				var fillErr error
				overCeiling.Do(func() { fillErr = fillWAL(ctx, f.pool, 24000) })
				if fillErr != nil {
					return fillErr
				}
			}
			return sleepFor(ctx, d)
		},
	})
	r, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)
	schemachange.PaceOverMark(r, math.MaxInt64, nil)
	_, err = r.Run(t.Context(), f.pool, f.target, f.liveStatement(t))

	var exceeded *decode.SlotLagExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, decode.SlotLagStreamBehind, exceeded.Cause, "the held confirmation is the stream's own lag")
	assert.Positive(t, exceeded.CopyPaced.Waits, "the copy was held back before the ceiling stopped the run")

	row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseCopying, row.Phase, "the run stopped in its copy")

	hidden.release(t)
}
