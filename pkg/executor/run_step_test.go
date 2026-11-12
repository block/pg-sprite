package executor_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// A half-set RunStep is a caller bug the AsStep entry points refuse with
// ErrInvariantViolation before anything else runs: a tracker with no step
// would report as step zero, which the snapshot reads as no step started,
// and a step with no tracker reports nothing the caller meant to observe.

func TestExecuteNativeAsStepRefusesATrackerWithNoStep(t *testing.T) {
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	err = executor.ExecuteNativeAsStep(t.Context(), nil, preflight.PreflightedTable{},
		statement.Statement{}, executor.Budget{LockTimeout: time.Second, StatementTimeout: time.Second},
		executor.DefaultRetryPolicy(), executor.RunStep{Tracker: tracker})
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

func TestRunSequenceAsStepRefusesAStepWithNoTracker(t *testing.T) {
	_, err := executor.RunSequenceAsStep(t.Context(), nil, preflight.PreflightedTable{},
		[]string{"ALTER TABLE s.t ADD COLUMN v int"}, executor.SequenceBudget{}, executor.DefaultRetryPolicy(),
		nil, nil, executor.RunStep{Step: 2})
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

// One sequence takes over an index or validates a constraint, never both:
// the diff plans the two as separate statements.
func TestRunSequenceAsStepRefusesBothAnAdoptionAndAValidation(t *testing.T) {
	_, err := executor.RunSequenceAsStep(t.Context(), nil, preflight.PreflightedTable{},
		[]string{"ALTER TABLE s.t ADD COLUMN v int"}, executor.SequenceBudget{}, executor.DefaultRetryPolicy(),
		&schemadiff.IndexAdoption{Index: "i"}, &schemadiff.ConstraintValidation{Constraint: "c"}, executor.RunStep{})
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}
