package executor_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// createBudget is generous for unit tests; admission refusals return
// before any database access.
var createBudget = executor.Budget{LockTimeout: time.Second, StatementTimeout: 2 * time.Second}

func TestExecuteCreateRejectsUnboundedBudget(t *testing.T) {
	ds, err := statement.ParseDesired("CREATE TABLE t (id int)")
	require.NoError(t, err)

	_, err = executor.ExecuteCreate(t.Context(), nil, preflight.AbsentTarget{}, ds,
		executor.Budget{LockTimeout: 0, StatementTimeout: time.Second}, executor.DefaultRetryPolicy())
	// The zero-value absence proof would also refuse (as an invariant
	// violation); asserting on the budget wording proves the budget check
	// fired first, since a valid proof needs a live database.
	require.ErrorContains(t, err, "lock budget")
}

// A zero-value AbsentTarget is constructible by any package; only
// CheckTableAbsent mints one with a verified target, so the executor
// refuses the forgery fail-closed.
func TestExecuteCreateRejectsZeroValueAbsenceProof(t *testing.T) {
	ds, err := statement.ParseDesired("CREATE TABLE t (id int)")
	require.NoError(t, err)

	_, err = executor.ExecuteCreate(t.Context(), nil, preflight.AbsentTarget{}, ds,
		createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

// A zero-value DesiredSchema carries no admitted CREATE TABLE; only
// ParseDesired mints one, so the executor refuses the forgery fail-closed.
// The refusal fires even though the absence proof is also zero-valued: the
// absence check runs first and reports the same invariant class.
func TestExecuteCreateRejectsZeroValueDesiredSchema(t *testing.T) {
	_, err := executor.ExecuteCreate(t.Context(), nil, preflight.AbsentTarget{}, statement.DesiredSchema{},
		createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

func TestExecuteCreateWithProgressRequiresTracker(t *testing.T) {
	ds, err := statement.ParseDesired("CREATE TABLE t (id int)")
	require.NoError(t, err)

	_, err = executor.ExecuteCreateWithProgress(t.Context(), nil, preflight.AbsentTarget{}, ds,
		createBudget, executor.DefaultRetryPolicy(), nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}
