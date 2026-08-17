package progress_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

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
