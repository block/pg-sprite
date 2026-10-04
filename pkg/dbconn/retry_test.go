package dbconn_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
)

func TestRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not retryable", nil, false},
		{"lock_not_available is retryable", &pgconn.PgError{Code: "55P03"}, true},
		{"deadlock_detected is retryable", &pgconn.PgError{Code: "40P01"}, true},
		{"serialization_failure is retryable", &pgconn.PgError{Code: "40001"}, true},
		{"connection_exception class is retryable", &pgconn.PgError{Code: "08006"}, true},
		{"syntax_error is not retryable", &pgconn.PgError{Code: "42601"}, false},
		{"undefined_table is not retryable", &pgconn.PgError{Code: "42P01"}, false},
		{"plain error is not retryable", errors.New("boom"), false},
		{"wrapped pg error is classified", wrap(&pgconn.PgError{Code: "55P03"}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dbconn.Retryable(tt.err))
		})
	}
}

func wrap(err error) error { return errors.Join(errors.New("context"), err) }

func TestRetry(t *testing.T) {
	t.Run("succeeds after transient failures", func(t *testing.T) {
		calls := 0
		err := dbconn.Retry(t.Context(), 3, time.Millisecond, func(context.Context) error {
			calls++
			if calls < 3 {
				return &pgconn.PgError{Code: "55P03"}
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
	})

	t.Run("returns non-transient error immediately", func(t *testing.T) {
		calls := 0
		wantErr := &pgconn.PgError{Code: "42601"}
		err := dbconn.Retry(t.Context(), 3, time.Millisecond, func(context.Context) error {
			calls++
			return wantErr
		})
		require.ErrorIs(t, err, wantErr)
		assert.Equal(t, 1, calls)
	})

	t.Run("exhausts attempts on persistent transient error", func(t *testing.T) {
		calls := 0
		err := dbconn.Retry(t.Context(), 3, time.Millisecond, func(context.Context) error {
			calls++
			return &pgconn.PgError{Code: "55P03"}
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "retries exhausted after 3 attempts")
		assert.Equal(t, 3, calls)
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := dbconn.Retry(ctx, 3, time.Millisecond, func(context.Context) error {
			return &pgconn.PgError{Code: "55P03"}
		})
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("rejects zero attempts", func(t *testing.T) {
		err := dbconn.Retry(t.Context(), 0, time.Millisecond, func(context.Context) error { return nil })
		require.Error(t, err)
	})
}
