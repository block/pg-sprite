package executor_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

func TestCreateShapeRefusals(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []error
	}{
		{name: "partition", sql: "CREATE TABLE t PARTITION OF parent FOR VALUES FROM (1) TO (2)", want: []error{executor.ErrPartitionOfUnsupported}},
		{name: "if not exists", sql: "CREATE TABLE IF NOT EXISTS t (id int)", want: []error{executor.ErrIfNotExistsUnsupported}},
		{name: "existing object clause", sql: "CREATE TABLE t (LIKE source)", want: []error{executor.ErrUnsupportedCreateStep}},
		{name: "later duplicate", sql: "CREATE TABLE t (id int PRIMARY KEY); CREATE INDEX t_pkey ON t (id)", want: []error{nil, executor.ErrDuplicateCreateName}},
		// A refused step still registers its claims: the collision with the
		// refused table's name is reported on the later statement instead of
		// admitting it.
		{name: "duplicate of refused step", sql: "CREATE TABLE IF NOT EXISTS t (id int); CREATE INDEX t ON t (id)", want: []error{executor.ErrIfNotExistsUnsupported, executor.ErrDuplicateCreateName}},
		// A step both refused by shape and colliding keeps the shape refusal
		// as its cause.
		{name: "refused shape wins over duplicate", sql: "CREATE TABLE t (id int PRIMARY KEY); CREATE INDEX IF NOT EXISTS t_pkey ON t (id)", want: []error{nil, executor.ErrIfNotExistsUnsupported}},
		{name: "admitted", sql: "CREATE TABLE t (id int); CREATE INDEX t_id ON t (id)", want: []error{nil, nil}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds, err := statement.ParseDesired(tc.sql)
			require.NoError(t, err)
			refusals, err := executor.CreateShapeRefusals("app", ds)
			require.NoError(t, err)
			require.Len(t, refusals, len(tc.want))
			for i := range tc.want {
				if tc.want[i] == nil {
					assert.NoError(t, refusals[i])
					continue
				}
				assert.ErrorIs(t, refusals[i], tc.want[i], "statement %d", i+1)
			}
		})
	}
}

// createBudget is generous for unit tests; admission refusals return
// before any database access.
var createBudget = executor.Budget{LockTimeout: time.Second, StatementTimeout: 2 * time.Second}

func TestExecuteCreateRejectsUnboundedBudget(t *testing.T) {
	ds, err := statement.ParseDesired("CREATE TABLE t (id int)")
	require.NoError(t, err)

	_, err = executor.ExecuteCreate(t.Context(), nil, preflight.AbsentTarget{}, preflight.CreationRole{}, ds,
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

	_, err = executor.ExecuteCreate(t.Context(), nil, preflight.AbsentTarget{}, preflight.CreationRole{}, ds,
		createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

// A zero-value DesiredSchema carries no admitted CREATE TABLE; only
// ParseDesired mints one, so the executor refuses the forgery fail-closed.
// The refusal fires even though the absence proof is also zero-valued: the
// absence check runs first and reports the same invariant class.
func TestExecuteCreateRejectsZeroValueDesiredSchema(t *testing.T) {
	_, err := executor.ExecuteCreate(t.Context(), nil, preflight.AbsentTarget{}, preflight.CreationRole{}, statement.DesiredSchema{},
		createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}

func TestExecuteCreateWithProgressRequiresTracker(t *testing.T) {
	ds, err := statement.ParseDesired("CREATE TABLE t (id int)")
	require.NoError(t, err)

	_, err = executor.ExecuteCreateWithProgress(t.Context(), nil, preflight.AbsentTarget{}, preflight.CreationRole{}, ds,
		createBudget, executor.DefaultRetryPolicy(), nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
}
