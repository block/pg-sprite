package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func TestBudgetVerdict(t *testing.T) {
	st := statement.Statement{
		SQL:    "ALTER TABLE billing.invoices ALTER COLUMN id TYPE bigint",
		Kind:   statement.KindAlterTable,
		Schema: "billing",
		Table:  "invoices",
	}

	t.Run("lock budget", func(t *testing.T) {
		v := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseLock, Budget: 3 * time.Second})
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, "billing.invoices", v.Table)
		assert.Contains(t, v.Detail, "lock was not granted within the 3s lock budget")
		assert.Contains(t, v.Detail, "nothing was executed")
	})

	t.Run("statement budget", func(t *testing.T) {
		v := budgetVerdict(st, &executor.BudgetError{Cause: executor.CauseStatement, Budget: 30 * time.Second})
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Contains(t, v.Detail, "cancelled after the 30s statement budget")
		assert.Contains(t, v.Detail, "ADD CONSTRAINT ... NOT VALID")
	})

	t.Run("unknown cause falls back to the error text", func(t *testing.T) {
		budgetErr := &executor.BudgetError{Budget: time.Second}
		v := budgetVerdict(st, budgetErr)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, budgetErr.Error(), v.Detail)
	})
}
