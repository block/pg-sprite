package cli

import (
	"fmt"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// parseMigrate runs args through the real command grammar so these tests
// exercise the same flag-to-field path production does.
func parseMigrate(t *testing.T, args ...string) *MigrateCmd {
	t.Helper()
	c := New()
	k, err := kong.New(c, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = k.Parse(append([]string{
		"migrate",
		"--url", "postgres://user@localhost:5432/app",
		"--alter", "ALTER TABLE t ADD COLUMN c int",
	}, args...))
	require.NoError(t, err)
	return &c.Migrate
}

func TestRetryFlagsWireIntoRetryPolicy(t *testing.T) {
	c := parseMigrate(t,
		"--lock-attempts", "5",
		"--lock-backoff", "250ms",
		"--lock-backoff-max", "2s",
	)
	// Full-struct equality: swapping the backoff fields, inverting the
	// zero-value fallback, or dropping the passthrough must all fail here.
	assert.Equal(t, executor.RetryPolicy{
		MaxAttempts:    5,
		InitialBackoff: 250 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
	}, c.retryPolicy())
}

func TestRetryPolicyDefaults(t *testing.T) {
	t.Run("kong defaults match the executor defaults", func(t *testing.T) {
		c := parseMigrate(t)
		assert.Equal(t, executor.DefaultRetryPolicy(), c.retryPolicy())
	})

	t.Run("zero-valued command falls back to the executor defaults", func(t *testing.T) {
		var c MigrateCmd
		assert.Equal(t, executor.DefaultRetryPolicy(), c.retryPolicy())
	})
}

func TestBudgetVerdict(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE billing.invoices ALTER COLUMN id TYPE bigint")
	require.NoError(t, err)

	t.Run("lock budget", func(t *testing.T) {
		v := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseLock, Budget: 3 * time.Second, Attempts: 3}, false, false)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, verdict.CauseLockBudget, v.Cause)
		assert.Equal(t, 3, v.Attempts, "the exhausted attempt count must reach the verdict")
		assert.Equal(t, "billing.invoices", v.Table)
		assert.False(t, v.Forced)
		assert.NotEmpty(t, v.Detail)
	})

	t.Run("statement budget", func(t *testing.T) {
		v := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseStatement, Budget: 30 * time.Second}, false, false)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, verdict.CauseStatementBudget, v.Cause)
		assert.NotEmpty(t, v.Detail)
	})

	t.Run("statement budget on the online idiom advises a larger budget", func(t *testing.T) {
		blind := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Minute}, false, false)
		online := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Minute}, false, true)
		assert.Equal(t, verdict.CauseStatementBudget, online.Cause)
		assert.NotEqual(t, blind.Detail, online.Detail,
			"a cancelled online idiom needs a larger budget, not the copy-and-swap advice")
	})

	t.Run("a forced refusal records the override", func(t *testing.T) {
		v := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Minute}, true, false)
		assert.True(t, v.Forced, "the machine-readable audit record must survive a refusal")
	})

	t.Run("unknown cause falls back to the error text", func(t *testing.T) {
		budgetErr := &executor.BudgetError{Budget: time.Second}
		v := budgetVerdict(st, budgetErr, false, false)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, verdict.CauseNone, v.Cause)
		assert.Equal(t, budgetErr.Error(), v.Detail)
	})
}

func TestExecRefusal(t *testing.T) {
	st, err := statement.ParseOne("CREATE INDEX CONCURRENTLY i ON billing.invoices (customer_id)")
	require.NoError(t, err)

	t.Run("nil error is not a refusal", func(t *testing.T) {
		_, refused := execRefusal(st, nil, false, false, false)
		assert.False(t, refused)
	})

	t.Run("a budget-cancelled attempt is a refusal", func(t *testing.T) {
		budgetErr := &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Minute}
		v, refused := execRefusal(st, fmt.Errorf("wrapped: %w", budgetErr), false, true, true)
		require.True(t, refused)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, verdict.CauseStatementBudget, v.Cause)
		assert.True(t, v.Forced)
	})

	t.Run("a substituted sequence's budget failure is operational", func(t *testing.T) {
		budgetErr := &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Minute}
		_, refused := execRefusal(st, fmt.Errorf("wrapped: %w", budgetErr), true, false, false)
		assert.False(t, refused, "a failed substituted step is an operational failure with a committed prefix")
	})

	t.Run("invalid-index debris is never a budget refusal", func(t *testing.T) {
		// The exact chain a budget-cancelled concurrent build that left an
		// invalid index produces: the buried *BudgetError must not map to a
		// budget verdict that conceals the operator-recovery outcome.
		invalid := &executor.InvalidIndexError{
			Schema:  "billing",
			Index:   "i",
			Build:   &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Minute},
			Cleanup: executor.ErrBuildLeftInvalidIndex,
		}
		stepErr := &executor.SequenceStepError{Step: 1, Total: 1, Err: invalid}
		_, refused := execRefusal(st, stepErr, false, false, true)
		assert.False(t, refused, "invalid-index debris needs an operator, not a budget verdict")
	})

	t.Run("static admission refusals map to a typed refusal verdict", func(t *testing.T) {
		for name, admissionErr := range map[string]error{
			"unsupported step": fmt.Errorf("sequence step 1 of 1: blocking CREATE INDEX: %w", executor.ErrUnsupportedSequenceStep),
			"unnamed index":    fmt.Errorf("sequence step 1 of 1: %w", executor.ErrUnnamedIndex),
			"if not exists":    fmt.Errorf("sequence step 1 of 1: %w", executor.ErrIfNotExistsUnsupported),
		} {
			t.Run(name, func(t *testing.T) {
				v, refused := execRefusal(st, admissionErr, true, false, false)
				require.True(t, refused, "an admission refusal decided before execution is a refusal verdict")
				assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
				assert.NotEmpty(t, v.Detail)
			})
		}
	})

	t.Run("an admission sentinel inside a step failure stays operational", func(t *testing.T) {
		stepErr := &executor.SequenceStepError{Step: 2, Total: 3, Err: executor.ErrUnnamedIndex}
		_, refused := execRefusal(st, stepErr, true, false, false)
		assert.False(t, refused, "a step failure means execution started; the committed prefix must surface")
	})

	t.Run("an operational server error is not a refusal", func(t *testing.T) {
		_, refused := execRefusal(st, fmt.Errorf("connection reset"), false, false, false)
		assert.False(t, refused)
	})
}
