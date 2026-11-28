package schemachange_test

import (
	"context"
	"sync"
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

// A copy far shorter than its checkpoint interval still saves the
// watermark it has landed, as soon as its first chunk lands, so a run
// stopped mid-copy resumes from that save rather than from the start of
// the key space. The interval is an hour and one chunk is pinned by a
// concurrent writer, so the copy can neither finish nor reach an interval
// save while the test waits; the run is stopped on the first save that
// records a landed watermark. The resumed run leaves every row at or
// below that watermark as the stopped run wrote it: the row at the
// watermark keeps the transaction id of the stopped run's insert.
func TestRunnerSavesTheFirstLandedChunkBeforeTheInterval(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	shadowName := schemachange.ShadowName(f.schema, "orders")
	pin := testutil.PinShadowKey(t, f.pool, pgx.Identifier{f.schema, shadowName}, 1450, 0)
	const pinnedChunkLower = 1401

	var (
		mu        sync.Mutex
		stoppedAt copier.Watermark
	)
	stopOnFirstLanded := func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCopying || !cp.Watermark.Valid() {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		stoppedAt = cp.Watermark
		return errStopHere
	}
	// The pinned chunk waits on the pin's transaction for as long as the
	// copier's lock timeout allows, far longer than the test waits for
	// the save.
	const pinnedChunkWait = time.Minute
	opts := schemachange.RunOptions{
		Copier: copier.Options{
			Workers:     2,
			LockTimeout: pinnedChunkWait,
			Clock:       pin.Clock(),
			Chunker:     copier.ChunkerOptions{InitialRows: 100, MinRows: 100, MaxRows: 100},
		},
		Checksum:           checksum.Options{Chunker: gateChunks},
		CheckpointInterval: time.Hour,
		AfterCheckpoint:    stopOnFirstLanded,
	}
	r, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)
	target := f.prove(t, "orders")
	st := f.alter(t, alter)

	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := r.Run(t.Context(), f.pool, target, st)
		results <- err
	})
	t.Cleanup(wg.Wait)

	// The first chunk lands within moments of the copy starting; an
	// interval save would come only after an hour.
	const firstSaveDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return stoppedAt.Valid()
	}, firstSaveDeadline, 20*time.Millisecond, "the copy should save its first landed watermark long before its interval")
	const stopPropagation = 5 * time.Second
	select {
	case err := <-results:
		require.ErrorIs(t, err, errStopHere)
	case <-time.After(stopPropagation):
		t.Fatalf("run did not stop within %s of the failed save; the copy was not cancelled with it", stopPropagation)
	}

	row := f.inspectRow(t, "orders")
	assert.Equal(t, checkpoint.PhaseCopying, row.Phase)
	assert.Equal(t, stoppedAt, row.Watermark, "the row carries the first save")
	landed := row.Watermark.Value()
	require.Less(t, landed, int64(pinnedChunkLower), "the watermark cannot pass the pinned chunk, so the save is partial")
	shadow := pgx.Identifier{f.schema, row.ShadowTable}.Sanitize()
	var stoppedXmin string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT xmin::text FROM "+shadow+" WHERE id = $1", landed).Scan(&stoppedXmin))
	pin.Release(t)

	f.assertResumedToDone(t, alter, schemachange.RunOptions{})
	var resumedXmin string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT xmin::text FROM "+pgx.Identifier{f.schema, "orders"}.Sanitize()+" WHERE id = $1", landed).Scan(&resumedXmin))
	assert.Equal(t, stoppedXmin, resumedXmin, "the resumed copy started above the saved watermark, so the row at it is the stopped run's insert")
}
