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

// hiddenCommit is one write to the workload table whose commit the source
// never makes visible while the run waits for its capture to catch up.
// The server names a standby that never connects, so a session that asks
// for synchronous commit waits in its commit until the test cancels the
// wait; every other session commits locally and never waits. The write
// lands once the run is catching up, so the stream carries it, and the
// hook returns once the writer's commit is waiting on the standby, so the
// commit record precedes the marker the run writes. The capture can never
// confirm past a commit the source hides, so the run stays in the marker
// wait with that one change buffered.
type hiddenCommit struct {
	f         reconcileFixture
	writer    *pgx.Conn
	writerPID int
	once      sync.Once
	wg        sync.WaitGroup
	err       error
}

// newHiddenCommit connects the writer on its own connection, outside the
// pool's statement timeout: its commit is meant to wait. f must come from
// newDecodingFixtureOn with synchronousStandbyThatNeverConnects.
func newHiddenCommit(t *testing.T, f reconcileFixture) *hiddenCommit {
	t.Helper()
	writer, err := pgx.Connect(t.Context(), f.cfg.URL)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, writer.Close(context.WithoutCancel(t.Context())))
	})
	h := &hiddenCommit{f: f, writer: writer}
	require.NoError(t, writer.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&h.writerPID))
	_, err = writer.Exec(t.Context(), `SET synchronous_commit = on`)
	require.NoError(t, err)
	t.Cleanup(func() {
		// A writer still waiting when the test fails is released so the
		// connection can close.
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `SELECT pg_cancel_backend($1)`, h.writerPID)
		assert.NoError(t, err)
		h.wg.Wait()
	})
	return h
}

// synchronousStandbyThatNeverConnects is the server setting hiddenCommit
// needs.
var synchronousStandbyThatNeverConnects = []string{"synchronous_commit=local", "synchronous_standby_names=blocked"}

// whenCatchingUp is the AfterCheckpoint hook that makes the write.
func (h *hiddenCommit) whenCatchingUp(t *testing.T) func(context.Context, checkpoint.Checkpoint) error {
	return func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCatchingUp {
			return nil
		}
		var hookErr error
		h.once.Do(func() {
			h.wg.Go(func() {
				_, h.err = h.writer.Exec(t.Context(), `UPDATE `+h.f.table.Qualified()+` SET amount = 99 WHERE id = 2`)
			})
			const commitWaits = 10 * time.Second
			waiting := assert.Eventually(t, func() bool {
				var waiting bool
				err := h.f.pool.QueryRow(t.Context(), `
					SELECT EXISTS (
						SELECT 1 FROM pg_catalog.pg_stat_activity
						WHERE pid = $1 AND wait_event = 'SyncRep')`, h.writerPID).Scan(&waiting)
				return err == nil && waiting
			}, commitWaits, 20*time.Millisecond, "the writer's commit waits on the standby")
			if !waiting {
				hookErr = errors.New("the writer's commit did not reach its standby wait")
			}
		})
		return hookErr
	}
}

// release ends the writer's wait. Cancelling a backend that is waiting
// for a synchronous standby ends the wait with a warning; the transaction
// has committed locally.
func (h *hiddenCommit) release(t *testing.T) {
	t.Helper()
	var cancelled bool
	require.NoError(t, h.f.pool.QueryRow(t.Context(), `SELECT pg_cancel_backend($1)`, h.writerPID).Scan(&cancelled))
	require.True(t, cancelled)
	h.wg.Wait()
	require.NoError(t, h.err, "the cancelled wait is a warning, not a failed commit")
}

// markerPoll is the live poll of the runs here: the marker wait is the one
// sleep of this length a live run takes, so a Sleep that sees it is inside
// the marker wait.
const markerPoll = 7 * time.Millisecond

// cancelOnceBuffered is a RunOptions.Sleep that, inside the marker wait,
// polls tracker and cancels the run on the poll that finds the capture
// holding the hidden change, after handing that snapshot to seen.
func cancelOnceBuffered(tracker *progress.Tracker, cancel context.CancelFunc, seen func(progress.Snapshot)) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		if d != markerPoll {
			return sleepFor(ctx, d)
		}
		snapshot, err := tracker.Progress(ctx)
		if err != nil {
			return err
		}
		if snapshot.Detail.Work != nil && snapshot.Detail.Work.ChangesBuffered == 1 {
			seen(snapshot)
			cancel()
			return ctx.Err()
		}
		return sleepFor(ctx, d)
	}
}

// A live run cancelled while it waits for the capture to catch up reports
// the wait it was in as a *CatchupWaitError carrying the catch-up's status,
// so the caller reads why the run had not caught up — here one change
// whose commit the source never made visible, since it waits on a
// synchronous standby that never connects — and still reads the error as
// the cancellation it is. The row stays catching up for a resume.
func TestRunnerCancelledInTheMarkerWaitReportsWhatTheCaptureWaitedOn(t *testing.T) {
	const seeded = 500
	f := newDecodingFixtureOn(t, seeded, synchronousStandbyThatNeverConnects)
	hidden := newHiddenCommit(t, f)

	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationCatchUp)
	tracker.StartStep(1, progress.OperationCatchUp, "")

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := liveRunOptions(schemachange.RunOptions{
		Live:            checksum.LiveOptions{Poll: markerPoll},
		Sleep:           cancelOnceBuffered(tracker, cancel, func(progress.Snapshot) {}),
		AfterCheckpoint: hidden.whenCatchingUp(t),
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

	hidden.release(t)
}

// A run handed the tracker of a longer run reports its marker wait as that
// run's step, with operation catch-up and the run's catch-up as the work
// source, so the change the capture holds shows as buffered. Here the run
// is step 2 of 3 of the caller's run. The catch-up stops being the work
// source when the wait ends, so a poll after the cancelled run returns
// carries no work.
func TestRunnerReportsItsCatchUpAsTheCallersStep(t *testing.T) {
	const seeded = 500
	f := newDecodingFixtureOn(t, seeded, synchronousStandbyThatNeverConnects)
	hidden := newHiddenCommit(t, f)

	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(3, progress.OperationAdmitting)

	// A run whose tracker never shows the buffered change is never
	// cancelled from inside its wait; the deadline ends it instead.
	const catchUpDeadline = time.Minute
	deadlineCtx, stop := context.WithTimeout(t.Context(), catchUpDeadline)
	defer stop()
	runCtx, cancel := context.WithCancel(deadlineCtx)
	defer cancel()
	var catchingUp progress.Snapshot
	opts := liveRunOptions(schemachange.RunOptions{
		Live:            checksum.LiveOptions{Poll: markerPoll},
		Sleep:           cancelOnceBuffered(tracker, cancel, func(s progress.Snapshot) { catchingUp = s }),
		AfterCheckpoint: hidden.whenCatchingUp(t),
		Tracker:         tracker,
		Step:            2,
	})
	r, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)
	_, err = r.Run(runCtx, f.pool, f.target, f.liveStatement(t))
	require.ErrorIs(t, err, context.Canceled, "the run is cancelled from inside its marker wait")

	assert.Equal(t, progress.PhaseRunning, catchingUp.Phase)
	assert.Equal(t, 2, catchingUp.Step)
	assert.Equal(t, 3, catchingUp.TotalSteps, "the caller's run keeps its own step count")
	assert.Equal(t, progress.OperationCatchUp, catchingUp.Detail.Operation)
	assert.Empty(t, catchingUp.Detail.Statement, "no one statement is what the catch-up runs")
	require.NotNil(t, catchingUp.Detail.Work)
	assert.Equal(t, uint64(1), catchingUp.Detail.Work.ChangesBuffered, "the hidden change is held in the catch-up's buffer")
	assert.Zero(t, catchingUp.Detail.Work.RowsCopied, "the catch-up reports none of the copy's counters")

	after, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.OperationCatchUp, after.Detail.Operation, "the caller finishes its run, not the Runner")
	assert.Nil(t, after.Detail.Work, "the catch-up stopped being the work source when its wait ended")

	hidden.release(t)
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
