package progress_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// fakeSource satisfies progress.WorkSource with a caller-supplied
// observation, standing in for the engine's copy step.
type fakeSource struct {
	work func(ctx context.Context) (progress.Work, error)
}

func (s fakeSource) Work(ctx context.Context) (progress.Work, error) { return s.work(ctx) }

// copyCounters are distinct per field so a swapped pair cannot pass.
var copyCounters = progress.Work{RowsCopied: 1200, RowsTotal: 5000, BytesCopied: 98304, BytesTotal: 409600}

// countingSource reports copyCounters and counts its polls.
func countingSource(polls *atomic.Int32) fakeSource {
	return fakeSource{work: func(context.Context) (progress.Work, error) {
		polls.Add(1)
		return copyCounters, nil
	}}
}

// forbiddenSource fails the test if it is ever polled.
func forbiddenSource(t *testing.T, why string) fakeSource {
	return fakeSource{work: func(context.Context) (progress.Work, error) {
		t.Errorf("work source polled: %s", why)
		return progress.Work{}, nil
	}}
}

// runningTrackerWithSource returns a tracker mid copy step, polling source.
func runningTrackerWithSource(t *testing.T, source progress.WorkSource) *progress.Tracker {
	t.Helper()
	tracker, err := progress.NewTracker(&fakeClock{now: time.Unix(100, 0)})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationCopy)
	tracker.StartStep(1, progress.OperationCopy, "INSERT INTO public.t_shadow (id) SELECT id FROM public.t WHERE id BETWEEN $1::bigint AND $2::bigint ON CONFLICT (id) DO NOTHING")
	tracker.SetWorkSource(source)
	return tracker
}

// The copy step's JSON is the adapter-facing contract for the copy
// operation: its operation value, and work carrying the engine's rows and
// bytes with the checksum and build counters at honest zero.
func TestSnapshotJSONShapeForACopyStep(t *testing.T) {
	var polls atomic.Int32
	clock := &fakeClock{now: time.Unix(100, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)
	tracker.Start(4, progress.OperationAdmitting)
	clock.now = clock.now.Add(2 * time.Second)
	const copySQL = "INSERT INTO public.t_shadow (id) SELECT id FROM public.t WHERE id BETWEEN $1::bigint AND $2::bigint ON CONFLICT (id) DO NOTHING"
	tracker.StartStep(2, progress.OperationCopy, copySQL)
	tracker.SetWorkSource(countingSource(&polls))
	clock.now = clock.now.Add(750 * time.Millisecond)

	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 5,
		"phase": "running",
		"step": 2,
		"total_steps": 4,
		"elapsed_ns": 2750000000,
		"step_elapsed_ns": 750000000,
		"detail": {
			"operation": "copy",
			"statement": "INSERT INTO public.t_shadow (id) SELECT id FROM public.t WHERE id BETWEEN $1::bigint AND $2::bigint ON CONFLICT (id) DO NOTHING",
			"active": true,
			"work": {
				"rows_copied": 1200,
				"rows_total": 5000,
				"bytes_copied": 98304,
				"bytes_total": 409600,
				"chunks_compared": 0,
				"rows_hashed": 0,
				"chunks_mismatched": 0,
				"chunks_repaired": 0,
				"blocks_done": 0,
				"blocks_total": 0,
				"tuples_done": 0,
				"tuples_total": 0,
				"lockers_total": 0,
				"lockers_done": 0
			}
		}
	}`, string(raw))
	assert.Equal(t, int32(1), polls.Load(), "one poll asks the source once")
}

// checksumCounters are distinct per field so a swapped pair cannot pass:
// a repair pass that compared three chunks, found two differing, recopied
// both, and has reread one of them so far.
var checksumCounters = progress.Work{ChunksCompared: 4, RowsHashed: 3500, ChunksMismatched: 2, ChunksRepaired: 2}

// The checksum step's JSON is the adapter-facing contract for the checksum
// operation: its operation value, and work carrying the engine's chunk and
// row counters with the copy and build counters at honest zero.
func TestSnapshotJSONShapeForAChecksumStep(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)
	tracker.Start(4, progress.OperationAdmitting)
	clock.now = clock.now.Add(2 * time.Second)
	tracker.StartStep(3, progress.OperationChecksum, "")
	tracker.SetWorkSource(fakeSource{work: func(context.Context) (progress.Work, error) { return checksumCounters, nil }})
	clock.now = clock.now.Add(750 * time.Millisecond)

	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 5,
		"phase": "running",
		"step": 3,
		"total_steps": 4,
		"elapsed_ns": 2750000000,
		"step_elapsed_ns": 750000000,
		"detail": {
			"operation": "checksum",
			"active": true,
			"work": {
				"rows_copied": 0,
				"rows_total": 0,
				"bytes_copied": 0,
				"bytes_total": 0,
				"chunks_compared": 4,
				"rows_hashed": 3500,
				"chunks_mismatched": 2,
				"chunks_repaired": 2,
				"blocks_done": 0,
				"blocks_total": 0,
				"tuples_done": 0,
				"tuples_total": 0,
				"lockers_total": 0,
				"lockers_done": 0
			}
		}
	}`, string(raw))
}

// Each poll asks the source afresh, so the counters a consumer sees are the
// source's current observation, not a value captured at registration.
func TestProgressPollsTheWorkSourceOnEveryObservation(t *testing.T) {
	var polls atomic.Int32
	source := fakeSource{work: func(context.Context) (progress.Work, error) {
		return progress.Work{RowsCopied: uint64(100 * polls.Add(1))}, nil
	}}
	tracker := runningTrackerWithSource(t, source)

	first, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	second, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	require.NotNil(t, first.Detail.Work)
	require.NotNil(t, second.Detail.Work)
	assert.Equal(t, uint64(100), first.Detail.Work.RowsCopied)
	assert.Equal(t, uint64(200), second.Detail.Work.RowsCopied)
}

// A source error leaves the snapshot with the last-known tracker state and
// no work, and the error reaches the poller — the same shape as a failed
// server progress query.
func TestProgressReturnsSnapshotAlongsideWorkSourceError(t *testing.T) {
	sourceErr := errors.New("shadow size lookup failed")
	tracker := runningTrackerWithSource(t, fakeSource{work: func(context.Context) (progress.Work, error) {
		return progress.Work{}, sourceErr
	}})

	s, err := tracker.Progress(t.Context())
	require.ErrorIs(t, err, sourceErr)
	assert.Equal(t, progress.PhaseRunning, s.Phase, "the snapshot must keep the last-known state on error")
	assert.Equal(t, 1, s.Step)
	assert.Equal(t, progress.OperationCopy, s.Detail.Operation)
	assert.Nil(t, s.Detail.Work, "a failed observation reports no counters rather than zeros")
}

// The source is polled only while the step runs: a terminal tracker reports
// its frozen snapshot without asking the source.
func TestFinishedTrackerDoesNotPollTheWorkSource(t *testing.T) {
	tracker := runningTrackerWithSource(t, forbiddenSource(t, "the run has finished"))
	tracker.Finish(nil)

	s, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, s.Phase)
	assert.Nil(t, s.Detail.Work)
}

// A later step never polls the earlier step's source: the copy's source
// must not report copy counters against the checksum step.
func TestStartStepDropsThePriorStepsWorkSource(t *testing.T) {
	tracker := runningTrackerWithSource(t, forbiddenSource(t, "the step it belonged to has ended"))
	tracker.StartStep(2, progress.OperationBrief, "ALTER TABLE public.t_shadow VALIDATE CONSTRAINT c")

	s, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, s.Step)
	assert.Nil(t, s.Detail.Work)
}

// A new run never polls the prior run's source, even when the prior run
// was abandoned without Finish or StopWorkSource: Start alone resets it.
func TestStartDropsThePriorRunsWorkSource(t *testing.T) {
	tracker := runningTrackerWithSource(t, forbiddenSource(t, "the run it belonged to was abandoned"))
	tracker.Start(1, progress.OperationCopy)

	s, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseRunning, s.Phase)
	assert.Nil(t, s.Detail.Work)
}

// The source is polled only while the tracker is running: a source wired
// before Start is not consulted for a pending snapshot.
func TestPendingTrackerDoesNotPollTheWorkSource(t *testing.T) {
	tracker, err := progress.NewTracker(&fakeClock{now: time.Unix(100, 0)})
	require.NoError(t, err)
	tracker.SetWorkSource(forbiddenSource(t, "the tracker has not started"))

	s, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhasePending, s.Phase)
	assert.Nil(t, s.Detail.Work)
}

// After StopWorkSource a poll reports the step without counters: the
// engine has released the state the source read.
func TestStopWorkSourceEndsPolling(t *testing.T) {
	var polls atomic.Int32
	tracker := runningTrackerWithSource(t, countingSource(&polls))
	_, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	tracker.StopWorkSource()

	s, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseRunning, s.Phase)
	assert.Nil(t, s.Detail.Work)
	assert.Equal(t, int32(1), polls.Load(), "the poll after StopWorkSource must not reach the source")
}

// A step's work comes from one place: registering a concurrent build drops
// the source, and registering a source drops the build.
func TestWorkSourceAndConcurrentBuildAreMutuallyExclusive(t *testing.T) {
	t.Run("build replaces source", func(t *testing.T) {
		tracker := runningTrackerWithSource(t, forbiddenSource(t, "a concurrent build replaced it"))
		tracker.SetConcurrentBuild(fakeSession{query: func(context.Context, string, ...any) pgx.Row {
			return fakeRow{scan: func(...any) error { return pgx.ErrNoRows }}
		}}, 4242)

		s, err := tracker.Progress(t.Context())
		require.NoError(t, err)
		assert.False(t, s.Detail.Active, "the build path answered: its row is gone")
		assert.Nil(t, s.Detail.Work)
	})
	t.Run("source replaces build", func(t *testing.T) {
		var polls atomic.Int32
		tracker := runningTrackerWithBuild(t, fakeSession{query: func(context.Context, string, ...any) pgx.Row {
			return fakeRow{scan: func(...any) error {
				t.Error("the reserved session was queried after a work source replaced the build")
				return pgx.ErrNoRows
			}}
		}})
		tracker.SetWorkSource(countingSource(&polls))

		s, err := tracker.Progress(t.Context())
		require.NoError(t, err)
		require.NotNil(t, s.Detail.Work)
		assert.Equal(t, copyCounters, *s.Detail.Work)
		assert.Equal(t, int32(1), polls.Load())
	})
	t.Run("source ends the build's ownership of its backend", func(t *testing.T) {
		tracker := runningTrackerWithBuild(t, neverQueried(t))
		tracker.SetWorkSource(countingSource(new(atomic.Int32)))
		require.ErrorIs(t, tracker.CancelBuild(t.Context()), progress.ErrNoActiveBuild,
			"a stale build PID must not be signalled once a source owns the step's work")

		tracker.StopWorkSource()
		s, err := tracker.Progress(t.Context())
		require.NoError(t, err)
		assert.Nil(t, s.Detail.Work, "stopping the source must not revive the replaced build")
	})
}

// Two concurrent pollers never reach the source at the same time, so a
// source may read state that is not safe for concurrent observation.
func TestProgressSerializesConcurrentWorkSourcePolls(t *testing.T) {
	firstEntered := make(chan struct{})
	overlap := make(chan struct{})
	release := make(chan struct{})
	var entries atomic.Int32
	tracker := runningTrackerWithSource(t, fakeSource{work: func(context.Context) (progress.Work, error) {
		switch entries.Add(1) {
		case 1:
			close(firstEntered)
		case 2:
			close(overlap)
		}
		<-release
		return copyCounters, nil
	}})

	var pollers sync.WaitGroup
	for range 2 {
		pollers.Go(func() {
			_, err := tracker.Progress(t.Context())
			assert.NoError(t, err)
		})
	}
	<-firstEntered
	secondPollerMustStillWait := time.After(100 * time.Millisecond)
	select {
	case <-overlap:
		t.Fatal("two pollers reached the work source concurrently")
	case <-secondPollerMustStillWait:
	}
	close(release)
	pollers.Wait()
	assert.Equal(t, int32(2), entries.Load(), "both pollers must complete, one after the other")
}

// assertHandoffDrainsInFlightObservation starts a poll that blocks inside the
// source, then runs handoff on another goroutine and requires that it does
// not return until the observation has left the source.
func assertHandoffDrainsInFlightObservation(t *testing.T, handoff func(*progress.Tracker)) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var observationFinished atomic.Bool
	tracker := runningTrackerWithSource(t, fakeSource{work: func(context.Context) (progress.Work, error) {
		close(entered)
		<-release
		observationFinished.Store(true)
		return copyCounters, nil
	}})

	var workers sync.WaitGroup
	workers.Go(func() {
		_, err := tracker.Progress(t.Context())
		assert.NoError(t, err)
	})
	<-entered

	handoffReturned := make(chan struct{})
	workers.Go(func() {
		handoff(tracker)
		assert.True(t, observationFinished.Load(),
			"the handoff must not return while an observation still reads the source")
		close(handoffReturned)
	})
	handoffMustStillBlock := time.After(100 * time.Millisecond)
	select {
	case <-handoffReturned:
		t.Fatal("the handoff returned while an observation was in flight")
	case <-handoffMustStillBlock:
	}
	close(release)
	workers.Wait()
}

// Every call that ends the source's ownership of the step's work drains an
// in-flight observation before returning: the engine releases the state the
// source reads as soon as the call returns, whether it stopped the source or
// replaced it with another source or a concurrent build.
func TestSourceHandoffsDrainInFlightObservation(t *testing.T) {
	t.Run("StopWorkSource", func(t *testing.T) {
		assertHandoffDrainsInFlightObservation(t, (*progress.Tracker).StopWorkSource)
	})
	t.Run("SetWorkSource", func(t *testing.T) {
		assertHandoffDrainsInFlightObservation(t, func(tracker *progress.Tracker) {
			tracker.SetWorkSource(countingSource(new(atomic.Int32)))
		})
	})
	t.Run("SetConcurrentBuild", func(t *testing.T) {
		assertHandoffDrainsInFlightObservation(t, func(tracker *progress.Tracker) {
			tracker.SetConcurrentBuild(neverQueried(t), 4242)
		})
	})
}

// The engine's own state updates never wait behind a slow source poll:
// polling is observability, not a gate on the copy. Only the handoffs that
// end a poll target's ownership wait; the resets that advance the run do not.
func TestStateMutatorsDoNotWaitForInFlightWorkSourcePoll(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	tracker := runningTrackerWithSource(t, fakeSource{work: func(context.Context) (progress.Work, error) {
		close(entered)
		<-release
		return copyCounters, nil
	}})

	var workers sync.WaitGroup
	defer workers.Wait()
	defer close(release)
	workers.Go(func() {
		_, err := tracker.Progress(t.Context())
		assert.NoError(t, err)
	})
	<-entered

	mutated := make(chan struct{})
	workers.Go(func() {
		tracker.SetAttempt(2)
		tracker.StartStep(2, progress.OperationBrief, "ALTER TABLE public.t ADD COLUMN c integer")
		tracker.Finish(nil)
		tracker.Start(1, progress.OperationCopy)
		close(mutated)
	})
	mutatorDeadline := time.After(5 * time.Second)
	select {
	case <-mutated:
	case <-mutatorDeadline:
		t.Fatal("a state mutator waited behind an in-flight work source poll")
	}
}
