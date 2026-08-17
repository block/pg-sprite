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

func TestExecuteWithLockRetry(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 4, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 25 * time.Millisecond}
	var sleeps []time.Duration
	attempts := 0
	err := executeWithLockRetry(t.Context(), policy, func(context.Context) error {
		attempts++
		if attempts < 4 {
			return &BudgetError{Cause: CauseLock, Budget: time.Millisecond}
		}
		return nil
	}, func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 4, attempts)
	assert.Equal(t, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond}, sleeps)
}

func TestExecuteWithLockRetryExhausted(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
	err := executeWithLockRetry(t.Context(), policy, func(context.Context) error {
		return &BudgetError{Cause: CauseLock, Budget: 5 * time.Millisecond}
	}, func(context.Context, time.Duration) error { return nil })
	var budgetErr *BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, 3, budgetErr.Attempts)
	assert.Contains(t, err.Error(), "after 3 bounded attempts")
}

func TestExecuteWithLockRetryDoesNotRetryOtherFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "statement timeout", err: &BudgetError{Cause: CauseStatement, Budget: time.Second}},
		{name: "operational error", err: &pgconn.PgError{Code: "42601"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			err := executeWithLockRetry(t.Context(), DefaultRetryPolicy(), func(context.Context) error {
				attempts++
				return tt.err
			}, func(context.Context, time.Duration) error { return errors.New("unexpected sleep") })
			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, 1, attempts)
		})
	}
}

func TestRetryPolicyRejectsUnboundedValues(t *testing.T) {
	tests := []RetryPolicy{
		{MaxAttempts: 0, InitialBackoff: time.Millisecond, MaxBackoff: time.Second},
		{MaxAttempts: 1, InitialBackoff: -time.Millisecond, MaxBackoff: time.Second},
		{MaxAttempts: 1, InitialBackoff: time.Second, MaxBackoff: time.Millisecond},
		// Retries with no backoff would re-enter the lock queue back-to-back.
		{MaxAttempts: 3, InitialBackoff: 0, MaxBackoff: 0},
		{MaxAttempts: 2, InitialBackoff: 0, MaxBackoff: time.Second},
	}
	for _, policy := range tests {
		require.Error(t, policy.validate())
	}
}

// A single attempt never sleeps, so it needs no backoff to be bounded.
func TestRetryPolicyAcceptsSingleAttemptWithoutBackoff(t *testing.T) {
	require.NoError(t, RetryPolicy{MaxAttempts: 1}.validate())
}

// The observer sees each attempt number before that attempt runs, so a
// progress tracker always reports the attempt actually executing.
func TestExecuteWithLockRetryObservedReportsEachAttempt(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
	var observed []int
	attempts := 0
	err := executeWithLockRetryObserved(t.Context(), policy, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return &BudgetError{Cause: CauseLock, Budget: time.Millisecond}
		}
		return nil
	}, func(context.Context, time.Duration) error { return nil }, func(attempt int) {
		require.Equal(t, attempts+1, attempt, "the observer must run before its attempt")
		observed = append(observed, attempt)
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, observed)
}

// A non-retryable failure still observes its one attempt: the tracker's
// attempt counter must match what ran, not what succeeded.
func TestExecuteWithLockRetryObservedReportsFailedOnlyAttempt(t *testing.T) {
	var observed []int
	err := executeWithLockRetryObserved(t.Context(), DefaultRetryPolicy(), func(context.Context) error {
		return &BudgetError{Cause: CauseStatement, Budget: time.Second}
	}, func(context.Context, time.Duration) error { return errors.New("unexpected sleep") }, func(attempt int) {
		observed = append(observed, attempt)
	})
	var budgetErr *BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, []int{1}, observed)
}
