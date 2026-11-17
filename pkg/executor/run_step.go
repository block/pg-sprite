package executor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// RunStep places one executor call inside a longer run whose tracker the
// caller owns: the caller has called Start for the whole run and calls
// Finish when the run ends, and this call is step Step of it. Every step
// the call executes — each statement of a safer sequence, a concurrent
// index build included — is reported to Tracker as step Step, with that
// statement's own operation and SQL, so an observer reads the run's
// position from the step and what is executing now from the detail. The
// call never calls Start or Finish: a front door that runs several
// statements under one tracker keeps one elapsed time and one terminal
// outcome across them. The zero value tracks nothing.
type RunStep struct {
	// Tracker is the run's tracker; nil tracks nothing.
	Tracker *progress.Tracker
	// Step is the 1-based step of the run this call is.
	Step int
}

// validate refuses a half-set RunStep: a tracker without a step would
// report as step zero, which the snapshot reads as no step started, and a
// step without a tracker is a caller that meant to track and wired nothing.
func (s RunStep) validate() error {
	if s.Tracker != nil && s.Step < 1 {
		return fmt.Errorf("%w: a tracked run step must be 1 or more, got %d", ErrInvariantViolation, s.Step)
	}
	if s.Tracker == nil && s.Step != 0 {
		return fmt.Errorf("%w: run step %d names no tracker", ErrInvariantViolation, s.Step)
	}
	return nil
}

// ExecuteNativeAsStep is ExecuteNative reported to the run step at: the
// attempt is step at.Step with operation progress.OperationOptimistic and
// its retry attempts recorded. A zero at runs exactly as ExecuteNative.
func ExecuteNativeAsStep(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, st statement.Statement, b Budget, retry RetryPolicy, at RunStep) error {
	if err := at.validate(); err != nil {
		return err
	}
	if at.Tracker != nil {
		at.Tracker.StartStep(at.Step, progress.OperationOptimistic, st.SQL())
	}
	return executeNative(ctx, pool, pt, st, b, retry, nil, nil, at.Tracker)
}

// RunSequenceAsStep is RunSequence reported to the run step at, taking
// over adopts and validating validates when they are set, as
// RunSequenceAdopting and RunSequenceValidating do; at most one of the two
// may be set. Each admitted step is reported as at.Step in turn, so a
// concurrent index build publishes the server's build counters under it. A
// zero at runs exactly as the untracked entry point for the same
// arguments.
func RunSequenceAsStep(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, steps []string, b SequenceBudget, retry RetryPolicy, adopts *schemadiff.IndexAdoption, validates *schemadiff.ConstraintValidation, at RunStep) (SequenceReport, error) {
	if err := at.validate(); err != nil {
		return SequenceReport{}, err
	}
	if adopts != nil && validates != nil {
		return SequenceReport{}, fmt.Errorf("%w: one sequence cannot carry both the adoption of index %s and the validation of constraint %s",
			ErrInvariantViolation, adopts.Index, validates.Constraint)
	}
	return runSequence(ctx, pool, pt, steps, b, retry, adopts, validates, at.Tracker, at.Step)
}

// ExecuteCreateAsSteps is ExecuteCreate reported to a run whose steps are
// the desired file's statements: the caller has started tracker for
// len(ds.Statements()) steps and finishes it, and the create reports its
// i-th statement as step i. A nil tracker runs exactly as ExecuteCreate.
func ExecuteCreateAsSteps(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, cr preflight.CreationRole, ds statement.DesiredSchema, b Budget, retry RetryPolicy, tracker *progress.Tracker) (SequenceReport, error) {
	return executeCreate(ctx, pool, at, cr, ds, b, retry, tracker)
}
