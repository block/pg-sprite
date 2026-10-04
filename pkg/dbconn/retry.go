package dbconn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE codes the engine treats as transient. lock_not_available is the
// expected outcome of every bounded lock acquisition (the lock-queue
// mitigation), so it must be retryable by design.
const (
	codeLockNotAvailable     = "55P03"
	codeDeadlockDetected     = "40P01"
	codeSerializationFailure = "40001"
	classConnectionException = "08"
)

// Retryable reports whether err is transient: a bounded lock wait that timed
// out, a deadlock or serialization failure, or a connection-level error.
func Retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeLockNotAvailable, codeDeadlockDetected, codeSerializationFailure:
			return true
		}
		return strings.HasPrefix(pgErr.Code, classConnectionException)
	}
	return pgconn.SafeToRetry(err)
}

// Retry runs fn up to attempts times with linear backoff, retrying only
// errors Retryable classifies as transient. Non-transient errors return
// immediately; context cancellation always wins.
func Retry(ctx context.Context, attempts int, backoff time.Duration, fn func(context.Context) error) error {
	if attempts < 1 {
		return fmt.Errorf("retry: attempts must be >= 1, got %d", attempts)
	}
	var last error
	for i := range attempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = fn(ctx)
		if last == nil {
			return nil
		}
		if !Retryable(last) {
			return last
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff * time.Duration(i+1)):
		}
	}
	return fmt.Errorf("retries exhausted after %d attempts: %w", attempts, last)
}
