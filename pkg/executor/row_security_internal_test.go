package executor

import (
	"context"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/statement"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRowSecurityErrorClassification(t *testing.T) {
	budget := Budget{LockTimeout: time.Second, StatementTimeout: time.Second}
	t.Run("server statement timeout", func(t *testing.T) {
		err := rowSecurityError(t.Context(), t.Context(), &pgconn.PgError{Code: "57014"}, budget)
		assert.Equal(t, CodeBudgetStatementExceeded, OutcomeCode(err))
	})
	t.Run("attempt deadline", func(t *testing.T) {
		attempt, cancel := context.WithDeadline(t.Context(), time.Time{})
		defer cancel()
		err := rowSecurityError(t.Context(), attempt, context.DeadlineExceeded, budget)
		assert.Equal(t, CodeBudgetStatementExceeded, OutcomeCode(err))
	})
	t.Run("caller cancellation is not budget exhaustion", func(t *testing.T) {
		caller, cancel := context.WithCancel(t.Context())
		cancel()
		err := rowSecurityError(caller, caller, context.Canceled, budget)
		assert.ErrorIs(t, err, context.Canceled)
		var budgetErr *BudgetError
		assert.NotErrorAs(t, err, &budgetErr)
	})
	t.Run("unrelated failure survives expired deadline", func(t *testing.T) {
		attempt, cancel := context.WithDeadline(t.Context(), time.Time{})
		defer cancel()
		err := rowSecurityError(t.Context(), attempt, ErrTableNotFound, budget)
		assert.Equal(t, CodeTableNotFound, OutcomeCode(err))
	})
	t.Run("commit uncertainty survives expired deadline", func(t *testing.T) {
		attempt, cancel := context.WithDeadline(t.Context(), time.Time{})
		defer cancel()
		err := rowSecurityError(t.Context(), attempt, &RowSecurityOutcomeUnknownError{Err: context.DeadlineExceeded}, budget)
		assert.Equal(t, CodeRowSecurityOutcomeUnknown, OutcomeCode(err))
	})
}

func TestRowSecurityAdmissionErrorsArePermanent(t *testing.T) {
	for _, cause := range []error{statement.ErrPolicyRelationDependency, statement.ErrRowSecurityDeclaration, &pgconn.PgError{Code: "42501"}} {
		err := rowSecurityError(t.Context(), t.Context(), cause, Budget{})
		assert.ErrorIs(t, err, cause)
		assert.Equal(t, CodeRowSecurityRefused, OutcomeCode(err))
		assert.True(t, OutcomeCode(err).Permanent())
	}
}

func TestRowSecurityPermissionDeniedRetainsPrivilegeCause(t *testing.T) {
	cause := &pgconn.PgError{Code: "42501"}
	err := rowSecurityError(t.Context(), t.Context(), cause, Budget{})
	require.ErrorIs(t, err, ErrRowSecurityRefused)
	require.ErrorIs(t, err, ErrRowSecurityPrivileges)
	require.ErrorIs(t, err, cause)
}
