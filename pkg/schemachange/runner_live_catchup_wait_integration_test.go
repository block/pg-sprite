package schemachange_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A live run cancelled while it waits for the capture to catch up reports
// the wait it was in as a *CatchupWaitError carrying the catch-up's status,
// so the caller reads why the run had not caught up — here one change
// whose commit the source never made visible, since it waits on a
// synchronous standby that never connects — and still reads the error as
// the cancellation it is. The row stays catching up for a resume.
//
// The server names a standby that never connects, so a session that asks
// for synchronous commit waits in its commit until the test cancels the
// wait; every other session commits locally and never waits. The
// writer's commit record is in the WAL before the run writes the marker
// it waits for, and the capture can never confirm past a commit the
// source hides, so the run is in the marker wait when it is cancelled.
func TestRunnerCancelledInTheMarkerWaitReportsWhatTheCaptureWaitedOn(t *testing.T) {
	const seeded = 500
	f := newDecodingFixtureOn(t, seeded, []string{"synchronous_commit=local", "synchronous_standby_names=blocked"})

	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationCatchUp)
	tracker.StartStep(1, progress.OperationCatchUp, "")

	// The writer is its own connection, outside the pool's statement
	// timeout: its commit is meant to wait.
	writer, err := pgx.Connect(t.Context(), f.cfg.URL)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, writer.Close(context.WithoutCancel(t.Context())))
	})
	var writerPID int
	require.NoError(t, writer.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&writerPID))
	_, err = writer.Exec(t.Context(), `SET synchronous_commit = on`)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var commitErr error
	t.Cleanup(func() {
		// A writer still waiting when the test fails is released so the
		// connection can close.
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `SELECT pg_cancel_backend($1)`, writerPID)
		assert.NoError(t, err)
		wg.Wait()
	})

	// The write lands once the run is catching up, so the stream carries
	// it; the hook returns once the writer's commit is waiting on the
	// standby, so the commit record precedes the marker the run writes.
	const commitWaits = 10 * time.Second
	var writeOnce sync.Once
	writeWhenCatchingUp := func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCatchingUp {
			return nil
		}
		var hookErr error
		writeOnce.Do(func() {
			wg.Go(func() {
				_, commitErr = writer.Exec(t.Context(), `UPDATE `+f.table.Qualified()+` SET amount = 99 WHERE id = 2`)
			})
			waiting := assert.Eventually(t, func() bool {
				var waiting bool
				err := f.pool.QueryRow(t.Context(), `
					SELECT EXISTS (
						SELECT 1 FROM pg_catalog.pg_stat_activity
						WHERE pid = $1 AND wait_event = 'SyncRep')`, writerPID).Scan(&waiting)
				return err == nil && waiting
			}, commitWaits, 20*time.Millisecond, "the writer's commit waits on the standby")
			if !waiting {
				hookErr = errors.New("the writer's commit did not reach its standby wait")
			}
		})
		return hookErr
	}

	// The run is cancelled from inside the marker wait, on the poll that
	// finds the capture holding the hidden change: the marker wait is the
	// one sleep of this length a live run takes.
	const markerPoll = 7 * time.Millisecond
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelOnceBuffered := func(ctx context.Context, d time.Duration) error {
		if d != markerPoll {
			return sleepFor(ctx, d)
		}
		snapshot, err := tracker.Progress(ctx)
		if err != nil {
			return err
		}
		if snapshot.Detail.Work != nil && snapshot.Detail.Work.ChangesBuffered == 1 {
			cancel()
			return ctx.Err()
		}
		return sleepFor(ctx, d)
	}
	opts := liveRunOptions(schemachange.RunOptions{
		Live:            checksum.LiveOptions{Poll: markerPoll},
		Sleep:           cancelOnceBuffered,
		AfterCheckpoint: writeWhenCatchingUp,
	})
	opts.Catchup.Tracker = tracker
	r, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)
	_, err = r.Run(runCtx, f.pool, f.target, f.liveStatement(t))

	var wait *schemachange.CatchupWaitError
	require.ErrorAs(t, err, &wait, "the run reports the wait it was cancelled in")
	require.ErrorIs(t, err, context.Canceled, "and still reads as the cancellation it is")
	assert.Equal(t, f.table.Schema, wait.Schema)
	assert.Equal(t, f.table.Table, wait.Table)
	assert.Equal(t, 1, wait.Status.Invisible, "the capture held the one change whose commit the source had not made visible")
	assert.Equal(t, 1, wait.Status.Buffered)
	assert.Zero(t, wait.Status.Deferred)
	assert.Less(t, wait.Status.Confirmed, wait.Marker, "the capture had not confirmed past the marker")
	assert.Positive(t, wait.ShortOfMarker(), "and the error says how far short of it the capture was")

	row, err := f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseCatchingUp, row.Phase, "the cancelled run left its row catching up")

	// Cancelling a backend that is waiting for a synchronous standby ends
	// the wait with a warning; the transaction has committed locally.
	var cancelled bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_cancel_backend($1)`, writerPID).Scan(&cancelled))
	require.True(t, cancelled)
	wg.Wait()
	require.NoError(t, commitErr, "the cancelled wait is a warning, not a failed commit")
}

// sleepFor waits for d or until ctx ends, the way a run without an
// injected Sleep waits.
func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
