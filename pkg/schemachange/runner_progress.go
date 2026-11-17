package schemachange

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/progress"
)

// validateTracker refuses a run tracker the Runner could not report to:
// a step it could not number, a step with no tracker, and a tracker beside
// a per-step one, which would make two work sources compete for one step.
func (o RunOptions) validateTracker() error {
	if o.Tracker == nil {
		if o.Step != 0 {
			return fmt.Errorf("%w: run step %d names no tracker", ErrInvalidOptions, o.Step)
		}
		return nil
	}
	if o.Step < 1 {
		return fmt.Errorf("%w: a tracked run step must be 1 or more, got %d", ErrInvalidOptions, o.Step)
	}
	if o.Copier.Tracker != nil || o.Checksum.Tracker != nil || o.Catchup.Tracker != nil {
		return fmt.Errorf("%w: the run's tracker reports every phase; leave Copier.Tracker, Checksum.Tracker and Catchup.Tracker nil", ErrInvalidOptions)
	}
	return nil
}

// startPhase reports the phase about to run as the run's step, when the
// run has a tracker. It drops the previous phase's work source, so the
// phase registers its own after this call.
func (r *Runner) startPhase(operation progress.Operation) {
	if r.opts.Tracker != nil {
		r.opts.Tracker.StartStep(r.opts.Step, operation, "")
	}
}

// copierOptions is the copier's options, with the run's tracker as the
// copy's tracker when the run has one.
func (r *Runner) copierOptions() copier.Options {
	o := r.opts.Copier
	if r.opts.Tracker != nil {
		o.Tracker = r.opts.Tracker
	}
	return o
}

// checksumOptions is the verification pass's options, with the run's
// tracker as the pass's tracker when the run has one.
func (r *Runner) checksumOptions() checksum.Options {
	o := r.opts.Checksum
	if r.opts.Tracker != nil {
		o.Tracker = r.opts.Tracker
	}
	return o
}

// reportCatchup makes the run's catch-up the work source of the catch-up
// phase, when the run has a tracker, and returns the fence that stops
// reporting it before the phase ends. The catch-up runs for the whole fill
// but owns the step only while the run waits on it alone; during the copy
// and the verification pass the copier and the verifier own it.
func (r *Runner) reportCatchup(catchup *applier.Catchup) (stop func()) {
	if r.opts.Tracker == nil {
		return func() {}
	}
	r.opts.Tracker.SetWorkSource(catchup)
	return r.opts.Tracker.StopWorkSource
}
