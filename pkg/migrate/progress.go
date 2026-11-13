package migrate

import (
	"errors"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/verdict"
)

// errNotExecuted is the outcome a tracked call records when it returns
// without an error but without executing what it was asked to: a refusal.
// The tracker distinguishes only success from the rest, and a refused call
// did not succeed at what it was asked to do.
var errNotExecuted = errors.New("the schema change was not executed")

// startProgress starts the call's tracker, when it has one, for total
// steps. Nothing has executed yet, so the operation is admitting.
func (o Options) startProgress(total int) {
	if o.Progress != nil {
		o.Progress.Start(total, progress.OperationAdmitting)
	}
}

// finishProgress records the call's outcome on its tracker, when it has
// one: finished for an executed outcome returned without an error, failed
// for everything else.
func (o Options) finishProgress(outcome verdict.Outcome, err error) {
	if o.Progress == nil {
		return
	}
	if err == nil && !executedOutcome(outcome) {
		err = errNotExecuted
	}
	o.Progress.Finish(err)
}

// executedOutcome reports whether outcome says the schema change committed.
func executedOutcome(outcome verdict.Outcome) bool {
	switch outcome {
	case verdict.OutcomeExecuted, verdict.OutcomeExecutedViaCopyAndSwap, verdict.OutcomeExecutedWithoutOnlineSafety:
		return true
	default:
		return false
	}
}

// startStep reports the statement's one bounded attempt as its step, for
// the paths that run one statement through an executor that does not
// report to a tracker itself.
func (o Options) startStep(step int, sql string) {
	if o.Progress != nil {
		o.Progress.StartStep(step, progress.OperationOptimistic, sql)
	}
}

// runStep is the executor's view of the statement's step: the zero
// RunStep, which tracks nothing, when the call has no tracker.
func (o Options) runStep(step int) executor.RunStep {
	if o.Progress == nil {
		return executor.RunStep{}
	}
	return executor.RunStep{Tracker: o.Progress, Step: step}
}
