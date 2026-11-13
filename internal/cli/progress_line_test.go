package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/progress"
)

func running(step, total int, d progress.Detail) progress.Snapshot {
	return progress.Snapshot{FormatVersion: progress.FormatVersion, Phase: progress.PhaseRunning, Step: step, TotalSteps: total, Detail: d}
}

// The copy of a desired-state run's second step: the step leads the line,
// then the percent, the rows against the catalog estimate, and the ETA.
func TestRenderProgressCopyInARunOfSeveralSteps(t *testing.T) {
	s := running(2, 3, progress.Detail{
		Operation: progress.OperationCopy,
		Active:    true,
		Work:      &progress.Work{RowsCopied: 1_200_000, RowsTotal: 2_900_000},
		Percent:   new(41.2),
		ETA:       new(3*time.Minute + 10*time.Second),
	})
	assert.Equal(t, "step 2/3  copy  41.2%  1.2M/2.9M rows  ETA 3m10s", renderProgress(s))
}

// A run of one step shows no step counter; a concurrent index build shows
// the server's phase, and its counters are already in the percent.
func TestRenderProgressConcurrentIndexBuild(t *testing.T) {
	s := running(1, 1, progress.Detail{
		Operation:   progress.OperationConcurrentIndex,
		Active:      true,
		ServerPhase: "building index: scanning table",
		Work:        &progress.Work{BlocksDone: 620, BlocksTotal: 1000},
		Percent:     new(62.0),
		ETA:         new(40 * time.Second),
	})
	assert.Equal(t, "concurrent index build  building index: scanning table  62.0%  ETA 40s", renderProgress(s))
}

// A catch-up has no total to measure against, so it shows the stream's lag
// and neither a percent nor an ETA.
func TestRenderProgressCatchUp(t *testing.T) {
	s := running(1, 1, progress.Detail{
		Operation: progress.OperationCatchUp,
		Active:    true,
		Work:      &progress.Work{LagBytes: 12 << 20, ChangesApplied: 40},
	})
	assert.Equal(t, "catch-up  lag 12.0 MiB", renderProgress(s))
}

func TestRenderProgressChecksum(t *testing.T) {
	s := running(1, 1, progress.Detail{
		Operation: progress.OperationChecksum,
		Active:    true,
		Work:      &progress.Work{RowsHashed: 522_000, RowsToHash: 2_900_000},
		Percent:   new(18.0),
		ETA:       new(time.Minute),
	})
	assert.Equal(t, "checksum  18.0%  522.0K/2.9M rows  ETA 1m", renderProgress(s))
}

// A native statement measures no work: the line names the operation only.
func TestRenderProgressNativeStatement(t *testing.T) {
	s := running(1, 1, progress.Detail{Operation: progress.OperationOptimistic, Active: true, Statement: "ALTER TABLE public.t ADD COLUMN c int"})
	assert.Equal(t, "statement", renderProgress(s))
}

// Before the first step starts the run is still admitting its statements.
func TestRenderProgressAdmitting(t *testing.T) {
	s := running(0, 2, progress.Detail{Operation: progress.OperationAdmitting, Active: true})
	assert.Equal(t, "step 0/2  admitting", renderProgress(s))
}

func TestRenderProgressValidateConstraint(t *testing.T) {
	s := running(2, 2, progress.Detail{Operation: progress.OperationValidate, Active: true})
	assert.Equal(t, "step 2/2  validate constraint", renderProgress(s))
}

// An operation this front door has no label for is shown by its contract
// token, never dropped.
func TestRenderProgressUnknownOperationShowsItsToken(t *testing.T) {
	s := running(1, 1, progress.Detail{Operation: progress.Operation("future-op"), Active: true})
	assert.Equal(t, "future-op", renderProgress(s))
}

// A copy whose total is not known yet and that has no percent or ETA shows
// its rows alone: unknown is never rendered as zero.
func TestRenderProgressOmitsUnknownPercentAndETA(t *testing.T) {
	s := running(1, 1, progress.Detail{
		Operation: progress.OperationCopy,
		Active:    true,
		Work:      &progress.Work{RowsCopied: 950},
	})
	assert.Equal(t, "copy  950 rows", renderProgress(s))
}

// Early in a step the percent is known but the ETA is not reported yet.
func TestRenderProgressOmitsETAWhilePercentIsKnown(t *testing.T) {
	s := running(1, 1, progress.Detail{
		Operation: progress.OperationCopy,
		Active:    true,
		Work:      &progress.Work{RowsCopied: 10_000, RowsTotal: 5_000_000},
		Percent:   new(0.2),
	})
	assert.Equal(t, "copy  0.2%  10.0K/5.0M rows", renderProgress(s))
}

// A step that has not reported work yet shows its operation alone.
func TestRenderProgressOmitsCounterWithoutWork(t *testing.T) {
	s := running(1, 1, progress.Detail{Operation: progress.OperationCatchUp, Active: true})
	assert.Equal(t, "catch-up", renderProgress(s))
}

// Pending and terminal snapshots render nothing: the verdict reports the
// outcome.
func TestRenderProgressSkipsSnapshotsThatAreNotRunning(t *testing.T) {
	for _, phase := range []progress.Phase{progress.PhasePending, progress.PhaseFinished, progress.PhaseFailed} {
		s := progress.Snapshot{Phase: phase, Step: 1, TotalSteps: 1, Detail: progress.Detail{Operation: progress.OperationCopy}}
		assert.Empty(t, renderProgress(s), phase)
	}
}

func TestFormatETA(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                 "0s",
		400 * time.Millisecond:            "0s",
		40 * time.Second:                  "40s",
		time.Minute:                       "1m",
		3*time.Minute + 10*time.Second:    "3m10s",
		2*time.Hour + 5*time.Minute:       "2h5m",
		time.Hour + 1500*time.Millisecond: "1h2s",
	} {
		assert.Equal(t, want, formatETA(d), d.String())
	}
}

func TestFormatCountAndBytes(t *testing.T) {
	assert.Equal(t, "999", formatCount(999))
	assert.Equal(t, "1.0K", formatCount(1_000))
	assert.Equal(t, "2.9M", formatCount(2_900_000))
	assert.Equal(t, "1.5B", formatCount(1_500_000_000))
	assert.Equal(t, "512 B", formatBytes(512))
	assert.Equal(t, "1.5 KiB", formatBytes(1536))
	assert.Equal(t, "2.0 GiB", formatBytes(2<<30))
}
