package cli

import (
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
		v := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseLock, Budget: 3 * time.Second, Attempts: 3})
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, verdict.CauseLockBudget, v.Cause)
		assert.Equal(t, 3, v.Attempts, "the exhausted attempt count must reach the verdict")
		assert.Equal(t, "billing.invoices", v.Table)
		assert.NotEmpty(t, v.Detail)
	})

	t.Run("statement budget", func(t *testing.T) {
		v := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseStatement, Budget: 30 * time.Second})
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, verdict.CauseStatementBudget, v.Cause)
		assert.NotEmpty(t, v.Detail)
	})

	t.Run("unknown cause falls back to the error text", func(t *testing.T) {
		budgetErr := &executor.BudgetError{Budget: time.Second}
		v := budgetVerdict(st, budgetErr)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, verdict.CauseNone, v.Cause)
		assert.Equal(t, budgetErr.Error(), v.Detail)
	})
}
