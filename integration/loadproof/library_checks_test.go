package loadproof_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The host cancels mid-copy only on a periodic save of a copy still
// running: the fresh row has landed nothing, the copy's last save carries
// the complete watermark, and a save in another phase is past the copy.
func TestMidCopySaveIsOnlyAPartialCopyingWatermark(t *testing.T) {
	cases := map[string]struct {
		cp   checkpoint.Checkpoint
		want bool
	}{
		"fresh row, nothing landed": {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying}, false},
		"partial watermark":         {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: copier.NewWatermark(2002)}, true},
		"complete watermark":        {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: copier.NewWatermark(math.MaxInt64)}, false},
		"catching up":               {checkpoint.Checkpoint{Phase: checkpoint.PhaseCatchingUp, Watermark: copier.NewWatermark(5)}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, midCopySave(tc.cp))
		})
	}
}

// The host cancels mid-verify only once the running checksum step has
// compared a chunk, so the cancel lands inside the pass.
func TestMidVerifyNeedsAComparedChunkInTheChecksumStep(t *testing.T) {
	running := func(op progress.Operation, work *progress.Work) progress.Snapshot {
		return progress.Snapshot{Phase: progress.PhaseRunning, Detail: progress.Detail{Operation: op, Work: work}}
	}
	cases := map[string]struct {
		s    progress.Snapshot
		want bool
	}{
		"checksum with a chunk compared": {running(progress.OperationChecksum, &progress.Work{ChunksCompared: 1}), true},
		"checksum before any chunk":      {running(progress.OperationChecksum, &progress.Work{}), false},
		"checksum with no counters yet":  {running(progress.OperationChecksum, nil), false},
		"the copy":                       {running(progress.OperationCopy, &progress.Work{ChunksCompared: 1}), false},
		"a finished run": {progress.Snapshot{Phase: progress.PhaseFinished,
			Detail: progress.Detail{Operation: progress.OperationChecksum, Work: &progress.Work{ChunksCompared: 1}}}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, midVerify(tc.s))
		})
	}
}

// The host stops once mid-copy, then once mid-verify, before the call
// that runs to its verdict.
func TestHostStopsMidCopyThenMidVerify(t *testing.T) {
	assert.Equal(t, []stopPoint{stopMidCopy, stopMidVerify}, hostStops())
}

// checkStoppedAt refuses an attempt the host's cancel did not stop before
// it reads the row: one that finished, one stopped by another cause, one
// with no cause of the host's, and one whose verdict is not a caller's
// cancel.
func TestCheckStoppedAtRefusesAnAttemptTheHostDidNotStop(t *testing.T) {
	cancelled := verdict.Verdict{Outcome: verdict.OutcomeFailed, Code: string(executor.CodeCancelledByCaller)}
	midCopyErr := fmt.Errorf("copy: %w (%w)", errCancelledMidCopy, context.Canceled)
	cases := map[string]attempt{
		"the run finished": {verdict: verdict.Verdict{Outcome: verdict.OutcomeExecutedViaCopyAndSwap}},
		"the verify stop's cause": {verdict: cancelled,
			err: fmt.Errorf("verify: %w (%w)", errCancelledMidVerify, context.Canceled)},
		"no cause of the host's":           {verdict: cancelled, err: context.Canceled},
		"the cause without a cancellation": {verdict: cancelled, err: fmt.Errorf("copy: %w", errCancelledMidCopy)},
		"a verdict coded otherwise": {verdict: verdict.Verdict{Outcome: verdict.OutcomeFailed, Code: "execution-failed"},
			err: midCopyErr},
		"a verdict that is not failed": {verdict: verdict.Verdict{Outcome: verdict.OutcomeExecutedViaCopyAndSwap,
			Code: string(executor.CodeCancelledByCaller)}, err: midCopyErr},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			var info attemptInfo
			err := checkStoppedAt(t.Context(), host{}, testutil.WorkloadTable{}, stopMidCopy, a, &info)
			require.ErrorIs(t, err, errNotStopped)
			assert.Empty(t, info.RowPhase, "the row is not read for an attempt the host did not stop")
		})
	}
}

// A call after a stop mid-copy resumed when it kept the stopped call's
// shadow and its first copying save is at or above the stopped watermark.
func TestCheckResumedAcceptsACopyThatCarriedOn(t *testing.T) {
	stopped := attemptInfo{Attempt: 1, ShadowOID: 16500, WatermarkAtStop: new(int64(2002))}
	for name, first := range map[string]int64{"at the stop": 2002, "beyond the stop": 9000, "complete": math.MaxInt64} {
		t.Run(name, func(t *testing.T) {
			resumed := attemptInfo{Attempt: 2, ShadowOID: 16500, Saves: []saveInfo{
				{Phase: checkpoint.PhaseCopying.String(), Watermark: new(first)},
				{Phase: checkpoint.PhaseVerifying.String(), Watermark: new(int64(math.MaxInt64))},
			}}
			assert.NoError(t, checkResumed(stopMidCopy, stopped, resumed))
		})
	}
}

// A call after a stop mid-copy that started over is refused: a new
// shadow, a first copying save that lands nothing as a fresh row's does,
// one below the stopped watermark, or no copying save at all.
func TestCheckResumedRefusesACopyThatStartedOver(t *testing.T) {
	stopped := attemptInfo{Attempt: 1, ShadowOID: 16500, WatermarkAtStop: new(int64(2002))}
	copying := checkpoint.PhaseCopying.String()
	verifying := saveInfo{Phase: checkpoint.PhaseVerifying.String(), Watermark: new(int64(math.MaxInt64))}
	cases := map[string]attemptInfo{
		"a new shadow": {Attempt: 2, ShadowOID: 16600, Saves: []saveInfo{{Phase: copying, Watermark: new(int64(9000))}, verifying}},
		"a fresh row":  {Attempt: 2, ShadowOID: 16500, Saves: []saveInfo{{Phase: copying}, verifying}},
		"below the stop": {Attempt: 2, ShadowOID: 16500, Saves: []saveInfo{
			{Phase: copying, Watermark: new(int64(400))}, verifying}},
		"no copying save": {Attempt: 2, ShadowOID: 16500, Saves: []saveInfo{verifying}},
	}
	for name, resumed := range cases {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, checkResumed(stopMidCopy, stopped, resumed), errNotResumed)
		})
	}
}

// A call after a stop mid-verify resumed when it kept the shadow and never
// went back to copying; one that copied again, or swapped in another
// table, is refused.
func TestCheckResumedHoldsAVerifyStopOutOfCopying(t *testing.T) {
	stopped := attemptInfo{Attempt: 2, ShadowOID: 16500}
	verifying := saveInfo{Phase: checkpoint.PhaseVerifying.String(), Watermark: new(int64(math.MaxInt64))}
	done := saveInfo{Phase: checkpoint.PhaseDone.String(), Watermark: new(int64(math.MaxInt64))}
	resumed := attemptInfo{Attempt: 3, ShadowOID: 16500, Saves: []saveInfo{verifying, done}}
	require.NoError(t, checkResumed(stopMidVerify, stopped, resumed))

	copiedAgain := attemptInfo{Attempt: 3, ShadowOID: 16500, Saves: []saveInfo{
		{Phase: checkpoint.PhaseCopying.String(), Watermark: new(int64(math.MaxInt64))}, verifying, done}}
	require.ErrorIs(t, checkResumed(stopMidVerify, stopped, copiedAgain), errNotResumed)

	otherTable := attemptInfo{Attempt: 3, ShadowOID: 16600, Saves: []saveInfo{verifying, done}}
	require.ErrorIs(t, checkResumed(stopMidVerify, stopped, otherTable), errNotResumed)
}
