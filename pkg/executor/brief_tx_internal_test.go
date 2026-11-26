// White-box tests for how a brief step's failure is typed.

package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAsBriefStepError covers the brief step's cancellation partition. The
// caller's ended context makes any cancellation the caller's, in either form
// it reaches the client. Under a live context an expired lock_timeout is the
// lock budget, and a 57014 is the statement budget only once the step ran for
// the whole budget. An earlier 57014, such as an operator's pg_cancel_backend,
// is an external cancellation and must not read as budget exhaustion.
func TestAsBriefStepError(t *testing.T) {
	const lockMS, statementMS = 500, 2000
	cancelled := &pgconn.PgError{Code: sqlstateQueryCanceled}
	live := t.Context()
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"server 57014", cancelled},
		{"client context.Canceled", context.Canceled},
		{"client context.DeadlineExceeded", context.DeadlineExceeded},
	} {
		t.Run("ended context with "+tc.name+" is the caller's cancellation", func(t *testing.T) {
			err := asBriefStepError(ended, tc.err, "attach", lockMS, statementMS, 3*time.Second)
			require.ErrorIs(t, err, ErrCancelledByCaller)
			require.ErrorIs(t, err, tc.err, "the original error stays in the chain")
			assert.False(t, errors.As(err, new(*BudgetError)))
			assert.NotErrorIs(t, err, ErrCancelledExternally)
		})
	}

	t.Run("57014 after the whole statement budget is budget exhaustion", func(t *testing.T) {
		err := asBriefStepError(live, cancelled, "attach", lockMS, statementMS, 2*time.Second)
		var budgetErr *BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, CauseStatement, budgetErr.Cause)
		assert.Equal(t, 2*time.Second, budgetErr.Budget)
		require.ErrorIs(t, err, cancelled)
	})

	t.Run("an early 57014 under a live context is an external cancellation", func(t *testing.T) {
		err := asBriefStepError(live, cancelled, "attach", lockMS, statementMS, 100*time.Millisecond)
		require.ErrorIs(t, err, ErrCancelledExternally)
		require.ErrorIs(t, err, cancelled)
		assert.False(t, errors.As(err, new(*BudgetError)), "an operator cancel must not read as budget exhaustion")
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		assert.Contains(t, err.Error(), "attach")
	})

	t.Run("lock_not_available is the lock budget", func(t *testing.T) {
		err := asBriefStepError(live, &pgconn.PgError{Code: sqlstateLockNotAvailable}, "attach", lockMS, statementMS, time.Second)
		var budgetErr *BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, CauseLock, budgetErr.Cause)
		assert.Equal(t, 500*time.Millisecond, budgetErr.Budget)
	})

	t.Run("another failure names the step", func(t *testing.T) {
		other := errors.New("boom")
		err := asBriefStepError(live, other, "attach", lockMS, statementMS, time.Second)
		require.ErrorIs(t, err, other)
		assert.Equal(t, "attach: boom", err.Error())
	})
}
