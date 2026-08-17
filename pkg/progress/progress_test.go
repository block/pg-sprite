package progress_test

import (
	"context"
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

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeRow satisfies pgx.Row with a caller-supplied Scan.
type fakeRow struct{ scan func(dest ...any) error }

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }

// fakeSession satisfies dbconn.RowQuerier with a caller-supplied query,
// standing in for the executor's reserved verdict session.
type fakeSession struct {
	query func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s fakeSession) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.query(ctx, sql, args...)
}

// runningTrackerWithBuild returns a tracker mid concurrent index build,
// polling against session.
func runningTrackerWithBuild(t *testing.T, session fakeSession) *progress.Tracker {
	t.Helper()
	tracker, err := progress.NewTracker(&fakeClock{now: time.Unix(100, 0)})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationConcurrentIndex)
	tracker.StartStep(1, progress.OperationConcurrentIndex)
	tracker.SetConcurrentBuild(session, 4242)
	return tracker
}

func TestTrackerReportsSequencePositionAndInjectedElapsed(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)

	tracker.Start(3, progress.OperationBrief)
	clock.now = clock.now.Add(2 * time.Second)
	tracker.StartStep(2, progress.OperationValidate)
	tracker.SetAttempt(2)
	clock.now = clock.now.Add(750 * time.Millisecond)

	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseRunning, snapshot.Phase)
	assert.Equal(t, 2, snapshot.Step)
	assert.Equal(t, 3, snapshot.TotalSteps)
	assert.Equal(t, 2750*time.Millisecond, snapshot.Elapsed)
	assert.Equal(t, 750*time.Millisecond, snapshot.StepElapsed)
	assert.Equal(t, progress.OperationValidate, snapshot.Detail.Operation)
	assert.Equal(t, 2, snapshot.Detail.Attempt)
	assert.Nil(t, snapshot.Detail.Work, "native progress must not fabricate copy counters")
}

func TestTrackerSequenceStepsAdvanceMonotonically(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)
	tracker.Start(3, progress.OperationBrief)

	for step := 1; step <= 3; step++ {
		tracker.StartStep(step, progress.OperationBrief)
		snapshot, progressErr := tracker.Progress(t.Context())
		require.NoError(t, progressErr)
		assert.Equal(t, step, snapshot.Step)
		assert.Equal(t, 3, snapshot.TotalSteps)
	}
	tracker.Finish(nil)
	terminal, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, terminal.Phase)
	assert.Equal(t, 3, terminal.Step)
}

func TestTrackerReportsTerminalState(t *testing.T) {
	tracker, err := progress.NewTracker(&fakeClock{now: time.Unix(100, 0)})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationOptimistic)
	tracker.Finish(nil)

	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, snapshot.Phase)
	assert.False(t, snapshot.Detail.Active)
}

func TestNewTrackerRequiresClock(t *testing.T) {
	_, err := progress.NewTracker(nil)
	require.Error(t, err)
}

// The server row's columns must land on the right Work fields — distinct
// values for every column so a swapped pair cannot pass.
func TestProgressMergesServerIndexBuildWork(t *testing.T) {
	session := fakeSession{query: func(context.Context, string, ...any) pgx.Row {
		return fakeRow{scan: func(dest ...any) error {
			*(dest[0].(*string)) = "building index"
			*(dest[1].(*uint64)) = 11 // blocks_done
			*(dest[2].(*uint64)) = 40 // blocks_total
			*(dest[3].(*uint64)) = 7  // tuples_done
			*(dest[4].(*uint64)) = 21 // tuples_total
			return nil
		}}
	}}
	tracker := runningTrackerWithBuild(t, session)

	s, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.True(t, s.Detail.Active)
	assert.Equal(t, "building index", s.Detail.ServerPhase)
	require.NotNil(t, s.Detail.Work)
	assert.Equal(t, uint64(11), s.Detail.Work.BlocksDone)
	assert.Equal(t, uint64(40), s.Detail.Work.BlocksTotal)
	assert.Equal(t, uint64(7), s.Detail.Work.TuplesDone)
	assert.Equal(t, uint64(21), s.Detail.Work.TuplesTotal)
	assert.Zero(t, s.Detail.Work.RowsCopied, "native progress must not fabricate copy counters")
}

// A build that has left the progress view is reported inactive, with no
// stale server detail attached.
func TestProgressClearsActiveWhenServerRowIsGone(t *testing.T) {
	session := fakeSession{query: func(context.Context, string, ...any) pgx.Row {
		return fakeRow{scan: func(...any) error { return pgx.ErrNoRows }}
	}}
	tracker := runningTrackerWithBuild(t, session)

	s, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.False(t, s.Detail.Active, "a vanished progress row means the build is no longer active")
	assert.Empty(t, s.Detail.ServerPhase)
	assert.Nil(t, s.Detail.Work)
}

// A transient query failure surfaces the error alongside the last-known
// tracker state, not a zero-valued snapshot.
func TestProgressReturnsSnapshotAlongsideQueryError(t *testing.T) {
	queryErr := errors.New("connection severed")
	session := fakeSession{query: func(context.Context, string, ...any) pgx.Row {
		return fakeRow{scan: func(...any) error { return queryErr }}
	}}
	tracker := runningTrackerWithBuild(t, session)

	s, err := tracker.Progress(t.Context())
	require.ErrorIs(t, err, queryErr)
	assert.Equal(t, progress.PhaseRunning, s.Phase, "the snapshot must keep the last-known state on error")
	assert.Equal(t, 1, s.Step)
	assert.Equal(t, 1, s.TotalSteps)
}

// Two concurrent pollers must never drive the reserved session at the same
// time: a single pgx connection is not safe for concurrent use.
func TestProgressSerializesConcurrentPollers(t *testing.T) {
	firstEntered := make(chan struct{})
	overlap := make(chan struct{})
	release := make(chan struct{})
	var entries atomic.Int32
	session := fakeSession{query: func(context.Context, string, ...any) pgx.Row {
		return fakeRow{scan: func(...any) error {
			switch entries.Add(1) {
			case 1:
				close(firstEntered)
			case 2:
				close(overlap)
			}
			<-release
			return pgx.ErrNoRows
		}}
	}}
	tracker := runningTrackerWithBuild(t, session)

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
		t.Fatal("two pollers reached the session concurrently")
	case <-secondPollerMustStillWait:
	}
	close(release)
	pollers.Wait()
	assert.Equal(t, int32(2), entries.Load(), "both pollers must complete, one after the other")
}

// StopConcurrentBuild must drain an in-flight observation before returning:
// the executor reuses the reserved session for its catalog verdict as soon
// as StopConcurrentBuild returns.
func TestStopConcurrentBuildDrainsInFlightObservation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var queryFinished atomic.Bool
	session := fakeSession{query: func(context.Context, string, ...any) pgx.Row {
		return fakeRow{scan: func(...any) error {
			close(entered)
			<-release
			queryFinished.Store(true)
			return pgx.ErrNoRows
		}}
	}}
	tracker := runningTrackerWithBuild(t, session)

	var workers sync.WaitGroup
	workers.Go(func() {
		_, err := tracker.Progress(t.Context())
		assert.NoError(t, err)
	})
	<-entered

	stopReturned := make(chan struct{})
	workers.Go(func() {
		tracker.StopConcurrentBuild()
		assert.True(t, queryFinished.Load(),
			"StopConcurrentBuild must not return while an observation still holds the session")
		close(stopReturned)
	})
	stopMustStillBlock := time.After(100 * time.Millisecond)
	select {
	case <-stopReturned:
		t.Fatal("StopConcurrentBuild returned while an observation was in flight")
	case <-stopMustStillBlock:
	}
	close(release)
	workers.Wait()
}

// The executor's own state updates must never wait behind a slow
// observation: polling is observability, not a gate on execution.
func TestStateMutatorsDoNotWaitForInFlightObservation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	session := fakeSession{query: func(context.Context, string, ...any) pgx.Row {
		return fakeRow{scan: func(...any) error {
			close(entered)
			<-release
			return pgx.ErrNoRows
		}}
	}}
	tracker := runningTrackerWithBuild(t, session)

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
		tracker.StartStep(1, progress.OperationConcurrentIndex)
		close(mutated)
	})
	mutatorDeadline := time.After(5 * time.Second)
	select {
	case <-mutated:
	case <-mutatorDeadline:
		t.Fatal("a state mutator waited behind an in-flight observation")
	}
}
