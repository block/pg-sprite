package checksum

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// runningTracker is a tracker inside a running checksum step, the state in
// which an orchestrator polls a pass.
func runningTracker(t *testing.T) *progress.Tracker {
	t.Helper()
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationChecksum)
	tracker.StartStep(1, progress.OperationChecksum, "")
	return tracker
}

// The counters accumulate what the pass tells them: compared chunks carry
// their source row counts, a mismatch, a repair and a reread count chunks,
// and nothing else moves. A fresh verifier reports all zeros.
func TestWorkAccumulatesTheCountersAPassReports(t *testing.T) {
	v := &Verifier{}
	work, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{}, work, "a verifier that has run no pass has counted nothing")

	v.countCompared(1000)
	v.countCompared(500)
	v.countMismatch()
	v.countRepaired(2)
	v.countReread()

	work, err = v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{
		ChunksCompared:   2,
		RowsHashed:       1500,
		ChunksMismatched: 1,
		ChunksRepaired:   2,
		ChunksReread:     1,
	}, work)
}

// report starts a pass: it zeroes the previous pass's counters and makes
// the verifier the tracker's work source until stop runs, after which a
// poll carries no engine work and the counters stay readable from the
// verifier itself.
func TestReportResetsTheCountersAndRegistersForThePass(t *testing.T) {
	tracker := runningTracker(t)
	v := &Verifier{opts: Options{Tracker: tracker}}
	v.countCompared(1000)
	v.countMismatch()

	stop, err := v.report()
	require.NoError(t, err)
	v.countCompared(250)

	polled, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	require.NotNil(t, polled.Detail.Work, "while a pass runs a poll asks the verifier")
	assert.Equal(t, progress.Work{ChunksCompared: 1, RowsHashed: 250}, *polled.Detail.Work, "the previous pass's chunk and mismatch were reset at report")

	stop()
	after, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a pass that returned is no longer the tracker's work source")
	direct, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 1, RowsHashed: 250}, direct, "the finished pass's counters survive until the next report")
}

// A verifier with no tracker still counts: report registers nowhere and
// stop has nothing to fence, so a library caller that does not observe
// progress pays nothing for it.
func TestReportWithoutATrackerOnlyResetsTheCounters(t *testing.T) {
	v := &Verifier{}
	v.countRepaired(3)

	stop, err := v.report()
	require.NoError(t, err)
	v.countCompared(10)
	stop()

	work, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 1, RowsHashed: 10}, work)
}

// A verifier runs one pass at a time: a report while a pass runs is refused
// with ErrPassRunning and changes nothing — the running pass keeps its
// counters and stays the tracker's work source — and the next report is
// accepted once the running pass's stop has run.
func TestReportRefusesAnOverlappingPass(t *testing.T) {
	tracker := runningTracker(t)
	v := &Verifier{opts: Options{Tracker: tracker}}
	stop, err := v.report()
	require.NoError(t, err)
	v.countCompared(1000)

	_, err = v.report()
	require.ErrorIs(t, err, ErrPassRunning)
	polled, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	require.NotNil(t, polled.Detail.Work, "the refused pass did not end the running pass's registration")
	assert.Equal(t, progress.Work{ChunksCompared: 1, RowsHashed: 1000}, *polled.Detail.Work, "the refused pass did not reset the running pass's counters")

	stop()
	next, err := v.report()
	require.NoError(t, err)
	defer next()
	work, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{}, work, "the next pass starts from zero once the previous one has stopped")
}
