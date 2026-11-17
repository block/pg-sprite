package progress_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// pollStep runs one step of operation against a source reporting work,
// polled stepElapsed after the step started on a fake clock.
func pollStep(t *testing.T, operation progress.Operation, work progress.Work, stepElapsed time.Duration) progress.Snapshot {
	t.Helper()
	clock := &fakeClock{now: time.Unix(100, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)
	tracker.Start(1, operation)
	tracker.StartStep(1, operation, "")
	tracker.SetWorkSource(fakeSource{work: func(context.Context) (progress.Work, error) { return work, nil }})
	clock.now = clock.now.Add(stepElapsed)
	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	return snapshot
}

// requirePercent fails unless the snapshot carries a percent, and returns it.
func requirePercent(t *testing.T, s progress.Snapshot) float64 {
	t.Helper()
	require.NotNil(t, s.Detail.Percent, "the step has a measurable total, so a percent is reported")
	return *s.Detail.Percent
}

// requireETA fails unless the snapshot carries an ETA, and returns it.
func requireETA(t *testing.T, s progress.Snapshot) time.Duration {
	t.Helper()
	require.NotNil(t, s.Detail.ETA, "the step has run long enough to project an ETA")
	return *s.Detail.ETA
}

// A copy that has landed a quarter of the catalog's row estimate in ten
// seconds reports 25% and projects thirty seconds more: the rate so far,
// scaled by the three quarters left.
func TestCopyReportsPercentAndETAFromRowsAgainstTheEstimate(t *testing.T) {
	s := pollStep(t, progress.OperationCopy, progress.Work{RowsCopied: 2500, RowsTotal: 10000}, 10*time.Second)
	assert.InDelta(t, 25.0, requirePercent(t, s), 0)
	assert.Equal(t, 30*time.Second, requireETA(t, s))
}

// The ETA comes from the tracker's injected clock: the same counters polled
// later project a longer remainder, because the rate so far is slower.
func TestETAScalesWithTheInjectedStepElapsed(t *testing.T) {
	work := progress.Work{RowsCopied: 2500, RowsTotal: 10000}
	assert.Equal(t, 30*time.Second, requireETA(t, pollStep(t, progress.OperationCopy, work, 10*time.Second)))
	assert.Equal(t, 60*time.Second, requireETA(t, pollStep(t, progress.OperationCopy, work, 20*time.Second)))
}

// A source table ANALYZE has never visited has no catalog row count, so the
// copy's rows_total is 0: there is nothing to measure rows_copied against,
// and the snapshot omits both figures rather than report 0% or divide by
// nothing.
func TestCopyWithoutARowEstimateReportsNoPercentOrETA(t *testing.T) {
	s := pollStep(t, progress.OperationCopy, progress.Work{RowsCopied: 300}, time.Minute)
	assert.Nil(t, s.Detail.Percent)
	assert.Nil(t, s.Detail.ETA)
}

// The catalog's estimate can be low — the source grew after ANALYZE — so a
// running copy can land more rows than rows_total. It never reads as 100%
// while the step runs, and the ETA is withdrawn because the total is now
// known to be short and the work left is unknown. Reaching the estimate
// exactly is the same case: rows may remain.
func TestCopyPastItsEstimateCapsBelowCompleteAndDropsTheETA(t *testing.T) {
	for _, copied := range []uint64{5000, 6500} {
		s := pollStep(t, progress.OperationCopy, progress.Work{RowsCopied: copied, RowsTotal: 5000}, time.Minute)
		assert.InDelta(t, 99.9, requirePercent(t, s), 0, "rows_copied %d of 5000", copied)
		assert.Nil(t, s.Detail.ETA, "rows_copied %d of 5000", copied)
	}
}

// Rounding does not carry a nearly finished copy to 100 either: 99.97% of
// the estimate reads as the running ceiling.
func TestCopyRoundingNeverReachesComplete(t *testing.T) {
	s := pollStep(t, progress.OperationCopy, progress.Work{RowsCopied: 9997, RowsTotal: 10000}, time.Minute)
	assert.InDelta(t, 99.9, requirePercent(t, s), 0)
}

// The ETA waits for signal: a step that has run under five seconds, or
// done under one percent of its work, reports its percent and no ETA.
func TestETAIsOmittedUntilTheStepHasEnoughSignal(t *testing.T) {
	early := pollStep(t, progress.OperationCopy, progress.Work{RowsCopied: 2500, RowsTotal: 10000}, 4*time.Second)
	assert.InDelta(t, 25.0, requirePercent(t, early), 0)
	assert.Nil(t, early.Detail.ETA, "four seconds is too short a rate to project from")

	barelyStarted := pollStep(t, progress.OperationCopy, progress.Work{RowsCopied: 50, RowsTotal: 10000}, time.Minute)
	assert.InDelta(t, 0.5, requirePercent(t, barelyStarted), 0)
	assert.Nil(t, barelyStarted.Detail.ETA, "half a percent is too little work to project from")

	nothingYet := pollStep(t, progress.OperationCopy, progress.Work{RowsTotal: 10000}, time.Minute)
	assert.InDelta(t, 0.0, requirePercent(t, nothingYet), 0, "a known total with nothing done is 0%, not unknown")
	assert.Nil(t, nothingYet.Detail.ETA)
}

// A checksum pass measures rows_hashed against rows_to_hash, the source's
// row estimate: 7000 of 10000 hashed in 14 seconds is 70% with six seconds
// left.
func TestChecksumReportsPercentAndETAFromRowsHashed(t *testing.T) {
	s := pollStep(t, progress.OperationChecksum, progress.Work{ChunksCompared: 7, RowsHashed: 7000, RowsToHash: 10000}, 14*time.Second)
	assert.InDelta(t, 70.0, requirePercent(t, s), 0)
	assert.Equal(t, 6*time.Second, requireETA(t, s))
}

// A checksum pass over a table with no catalog row count has no total, and
// the copy's rows_total is not borrowed in its place: the pass reports no
// percent or ETA.
func TestChecksumWithoutARowEstimateReportsNoPercentOrETA(t *testing.T) {
	s := pollStep(t, progress.OperationChecksum, progress.Work{ChunksCompared: 3, RowsHashed: 2500, RowsTotal: 2500}, time.Minute)
	assert.Nil(t, s.Detail.Percent)
	assert.Nil(t, s.Detail.ETA)
}

// A catch-up runs until its owner cuts over, so it has no total: however
// far it has got, the snapshot carries its counters and no percent or ETA.
func TestCatchUpReportsNoPercentOrETA(t *testing.T) {
	s := pollStep(t, progress.OperationCatchUp, progress.Work{ChangesApplied: 8200, ChangesBuffered: 37, LagBytes: 65536}, time.Minute)
	require.NotNil(t, s.Detail.Work)
	assert.Nil(t, s.Detail.Percent)
	assert.Nil(t, s.Detail.ETA)
}

// A source error leaves the snapshot without work, so nothing is derived.
func TestWorkSourceErrorReportsNoPercent(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)
	tracker.Start(1, progress.OperationCopy)
	tracker.StartStep(1, progress.OperationCopy, "")
	tracker.SetWorkSource(fakeSource{work: func(context.Context) (progress.Work, error) {
		return progress.Work{}, errors.New("measure failed")
	}})
	clock.now = clock.now.Add(time.Minute)
	s, err := tracker.Progress(t.Context())
	require.Error(t, err)
	assert.Nil(t, s.Detail.Percent)
	assert.Nil(t, s.Detail.ETA)
}

// A terminal snapshot stays frozen as Finish left it: no work, so no
// percent or ETA, however long after the step a poll lands.
func TestTerminalSnapshotCarriesNoPercentOrETA(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)
	tracker.Start(1, progress.OperationCopy)
	tracker.StartStep(1, progress.OperationCopy, "")
	tracker.SetWorkSource(fakeSource{work: func(context.Context) (progress.Work, error) {
		return progress.Work{RowsCopied: 2500, RowsTotal: 10000}, nil
	}})
	clock.now = clock.now.Add(10 * time.Second)
	running, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	require.NotNil(t, running.Detail.Percent)

	tracker.Finish(nil)
	clock.now = clock.now.Add(time.Minute)
	finished, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, finished.Detail.Percent)
	assert.Nil(t, finished.Detail.ETA)
}
