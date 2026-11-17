package executor_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// The *WithProgress entry points exist for callers that poll; a nil tracker
// is a caller bug they must refuse with ErrInvariantViolation before anything
// else runs — never a panic, and never a silent fallback to unobserved
// execution — so OutcomeCode classifies it as a programmer error, not an
// operational failure to retry.

func TestExecuteNativeWithProgressRequiresTracker(t *testing.T) {
	err := executor.ExecuteNativeWithProgress(t.Context(), nil, preflight.PreflightedTable{},
		statement.Statement{}, executor.Budget{LockTimeout: time.Second, StatementTimeout: time.Second},
		executor.DefaultRetryPolicy(), nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

func TestExecuteNativeAdoptingWithProgressRequiresTracker(t *testing.T) {
	err := executor.ExecuteNativeAdoptingWithProgress(t.Context(), nil, preflight.PreflightedTable{},
		statement.Statement{}, executor.Budget{LockTimeout: time.Second, StatementTimeout: time.Second},
		executor.DefaultRetryPolicy(),
		schemadiff.IndexAdoption{Index: "t_email_key", Def: "CREATE UNIQUE INDEX t_email_key ON t USING btree (email)"}, nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

func TestExecuteNativeValidatingWithProgressRequiresTracker(t *testing.T) {
	err := executor.ExecuteNativeValidatingWithProgress(t.Context(), nil, preflight.PreflightedTable{},
		statement.Statement{}, executor.Budget{LockTimeout: time.Second, StatementTimeout: time.Second},
		executor.DefaultRetryPolicy(),
		schemadiff.ConstraintValidation{Constraint: "t_qty_check", Def: "CHECK ((qty > 0)) NOT VALID"}, nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

func TestExecuteIndexRenameWithProgressRequiresTracker(t *testing.T) {
	err := executor.ExecuteIndexRenameWithProgress(t.Context(), nil, preflight.PreflightedTable{},
		statement.Statement{}, executor.Budget{LockTimeout: time.Second, StatementTimeout: time.Second},
		executor.DefaultRetryPolicy(), nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

// requireOneStepSnapshot reads tracker after a single-statement
// *WithProgress call returned and checks the terminal snapshot every such
// entry point leaves: the given phase, step 1 of 1 under operation, the
// exact submitted SQL, and attempts bounded attempts observed — zero when
// the call refused before any attempt ran.
func requireOneStepSnapshot(t *testing.T, tracker *progress.Tracker, phase progress.Phase, operation progress.Operation, sql string, attempts int) {
	t.Helper()
	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, phase, snapshot.Phase)
	assert.Equal(t, 1, snapshot.Step)
	assert.Equal(t, 1, snapshot.TotalSteps)
	assert.Equal(t, operation, snapshot.Detail.Operation)
	assert.Equal(t, sql, snapshot.Detail.Statement)
	assert.Equal(t, attempts, snapshot.Detail.Attempt)
	assert.False(t, snapshot.Detail.Active)
}

func TestRunSequenceWithProgressRequiresTracker(t *testing.T) {
	_, err := executor.RunSequenceWithProgress(t.Context(), nil, preflight.PreflightedTable{},
		[]string{"ALTER TABLE s.t ADD COLUMN v int"},
		executor.SequenceBudget{}, executor.DefaultRetryPolicy(), nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

func TestBuildIndexConcurrentlyWithProgressRequiresTracker(t *testing.T) {
	_, err := executor.BuildIndexConcurrentlyWithProgress(t.Context(), nil,
		"CREATE INDEX CONCURRENTLY i ON s.t (c)", executor.ConcurrentBudget{Overall: time.Minute}, nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}
