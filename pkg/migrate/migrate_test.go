package migrate

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func TestGate(t *testing.T) {
	parse := func(t *testing.T, sql string) statement.Statement {
		t.Helper()
		st, err := statement.ParseOne(sql)
		require.NoError(t, err)
		return st
	}

	t.Run("supported kinds pass", func(t *testing.T) {
		for name, sql := range map[string]string{
			"alter table":  "ALTER TABLE billing.invoices ADD COLUMN age int",
			"create index": "CREATE INDEX i ON billing.invoices (age)",
		} {
			t.Run(name, func(t *testing.T) {
				_, refused := Gate(parse(t, sql))
				assert.False(t, refused)
			})
		}
	})

	t.Run("blocking index maintenance points at the concurrent idiom", func(t *testing.T) {
		v, refused := Gate(parse(t, "DROP INDEX billing.i"))
		require.True(t, refused)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonIndexStatement, v.Reason)
		assert.Equal(t, "DROP INDEX CONCURRENTLY", v.SaferIdiom)
	})

	t.Run("already-concurrent maintenance carries no safer idiom", func(t *testing.T) {
		v, refused := Gate(parse(t, "DROP INDEX CONCURRENTLY billing.i"))
		require.True(t, refused)
		assert.Equal(t, verdict.ReasonIndexStatement, v.Reason)
		assert.Empty(t, v.SaferIdiom,
			"suggesting the submitted statement back would loop a resubmitting automation")
	})

	t.Run("create table points at the declarative front door", func(t *testing.T) {
		v, refused := Gate(parse(t, "CREATE TABLE t (id int)"))
		require.True(t, refused)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
		assert.Empty(t, v.SaferIdiom,
			"the library names the concept; each front door attaches its own actionable spelling")
	})

	t.Run("other kinds are unsupported", func(t *testing.T) {
		v, refused := Gate(parse(t, "DROP TABLE t"))
		require.True(t, refused)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
		assert.Empty(t, v.SaferIdiom)
	})
}

func TestRunRejectsUnrunnableOptions(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE billing.invoices ADD COLUMN age int")
	require.NoError(t, err)

	// Options validation happens before any database work, so no pool is
	// needed: the zero value must be rejected by field name on every path,
	// not only the paths that reach the size guard.
	v, err := Run(t.Context(), nil, st, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MaxTableSizeBytes",
		"the rejection must name the Options field, not an internal preflight concept")
	assert.Equal(t, verdict.Verdict{}, v, "an error before a verdict carries a zero verdict")
}

func TestDefaultOptionsIsRunnable(t *testing.T) {
	opts := DefaultOptions()
	assert.NoError(t, opts.validate())
	assert.Equal(t, executor.DefaultRetryPolicy(), opts.Retry)
}

func TestOptionsRetryDefaults(t *testing.T) {
	t.Run("zero value falls back to the executor defaults", func(t *testing.T) {
		var o Options
		assert.Equal(t, executor.DefaultRetryPolicy(), o.retry())
	})

	t.Run("a configured policy passes through untouched", func(t *testing.T) {
		policy := executor.RetryPolicy{
			MaxAttempts:    5,
			InitialBackoff: 250 * time.Millisecond,
			MaxBackoff:     2 * time.Second,
		}
		assert.Equal(t, policy, Options{Retry: policy}.retry())
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

func TestFailureVerdict(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE billing.invoices ALTER COLUMN status SET NOT NULL")
	require.NoError(t, err)

	t.Run("a mid-sequence failure discloses the step and the committed prefix", func(t *testing.T) {
		stepErr := &executor.SequenceStepError{
			Step:  2,
			Total: 4,
			Kind:  executor.StepValidateConstraint,
			SQL:   "ALTER TABLE billing.invoices VALIDATE CONSTRAINT c",
			Err:   fmt.Errorf("server error"),
		}
		rep := executor.SequenceReport{Steps: []executor.StepReport{
			{SQL: "ALTER TABLE billing.invoices ADD CONSTRAINT c CHECK (status IS NOT NULL) NOT VALID", Kind: executor.StepBrief},
		}}
		v := failureVerdict(st, fmt.Errorf("wrapped: %w", stepErr), rep, false)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
		assert.Equal(t, 2, v.FailedStep)
		assert.Equal(t, stepErr.SQL, v.FailedStepSQL)
		assert.Equal(t, []string{rep.Steps[0].SQL}, v.ExecutedSQL,
			"the committed prefix is what distinguishes partial state from nothing happened")
		assert.Equal(t, "billing.invoices", v.Table)
		assert.False(t, v.Forced)
	})

	t.Run("the failed step's typed cause maps to its own stable code", func(t *testing.T) {
		stepErr := &executor.SequenceStepError{
			Step: 2, Total: 4, Kind: executor.StepValidateConstraint,
			SQL: "ALTER TABLE billing.invoices VALIDATE CONSTRAINT c",
			Err: &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Minute},
		}
		v := failureVerdict(st, stepErr, executor.SequenceReport{}, false)
		assert.Equal(t, string(executor.CodeBudgetStatementExceeded), v.Code)
		assert.Empty(t, v.ExecutedSQL, "an empty committed prefix means nothing committed")
	})

	t.Run("a non-sequence failure carries no step and an empty prefix", func(t *testing.T) {
		v := failureVerdict(st, fmt.Errorf("server error"), executor.SequenceReport{}, true)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
		assert.Zero(t, v.FailedStep)
		assert.Empty(t, v.FailedStepSQL)
		assert.Empty(t, v.ExecutedSQL)
		assert.True(t, v.Forced, "the machine-readable audit record must survive a failure")
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
