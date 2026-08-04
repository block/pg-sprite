// Package executor runs schema changes against the database. In Phase 1 it
// holds the optimistic front door: attempt the change directly under a tight
// lock_timeout and statement_timeout so it can only succeed if it is
// effectively an instant / in-place change. A budget overrun cancels the
// statement cleanly — nothing is executed — and surfaces as a typed
// BudgetError the caller turns into a not-native-safe verdict.
//
// This is a safety-critical core package: see SAFETY.md. It never trusts the
// caller's classification — its own protection is the budget, applied with
// SET LOCAL inside the attempt's transaction regardless of session defaults.
package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
)

// SQLSTATE codes the attempt maps to budget outcomes. Postgres errors are
// matched by SQLSTATE, never by message text.
const (
	sqlstateLockNotAvailable = "55P03" // lock_timeout expired
	sqlstateQueryCanceled    = "57014" // statement_timeout expired
)

// BudgetCause says which budget the optimistic attempt exceeded.
type BudgetCause int

// The two budgets an attempt runs under.
const (
	// CauseLock means the lock was not granted within lock_timeout: the
	// table is too contended for a blind attempt right now.
	CauseLock BudgetCause = iota + 1
	// CauseStatement means the statement ran past statement_timeout: the
	// change is doing real work (a rewrite), not an in-place catalog change.
	CauseStatement
)

// String returns the human-readable budget name.
func (c BudgetCause) String() string {
	switch c {
	case CauseLock:
		return "lock budget"
	case CauseStatement:
		return "statement budget"
	default:
		return "unknown budget"
	}
}

// BudgetError reports that the optimistic attempt exceeded one of its budgets
// and was cancelled cleanly: the statement did not execute and the
// transaction rolled back. It is a refusal input, not an operational failure.
type BudgetError struct {
	// Cause is the budget that was exceeded.
	Cause BudgetCause
	// Budget is the configured limit for that cause.
	Budget time.Duration
}

// Error implements the error interface.
func (e *BudgetError) Error() string {
	return fmt.Sprintf("optimistic attempt exceeded its %s (%s) and was cancelled", e.Cause, e.Budget)
}

// Budget bounds one optimistic attempt. Both limits must be positive: an
// unbounded attempt is exactly the stall the front door exists to prevent.
type Budget struct {
	// LockTimeout bounds how long the attempt may wait in the lock queue.
	LockTimeout time.Duration
	// StatementTimeout bounds how long the statement may run once started.
	StatementTimeout time.Duration
}

// validate rejects budgets that would leave the attempt unbounded.
func (b Budget) validate() error {
	// INV: LK-2 — the attempt is bounded by construction; a zero or negative
	// timeout would disable the corresponding PostgreSQL limit.
	if b.LockTimeout <= 0 {
		return fmt.Errorf("lock budget must be positive, got %s", b.LockTimeout)
	}
	if b.StatementTimeout <= 0 {
		return fmt.Errorf("statement budget must be positive, got %s", b.StatementTimeout)
	}
	return nil
}

// AttemptNative runs sql once, directly, inside a transaction bounded by b.
// The table must have passed preflight — the proof parameter makes skipping
// the size guard unrepresentable. On success the change is committed: it was
// effectively instant. If a budget is exceeded the statement is cancelled by
// the server, the transaction rolls back, and a *BudgetError is returned.
// Any other failure is surfaced as an operational error.
func AttemptNative(ctx context.Context, pool *pgxpool.Pool, _ preflight.PreflightedTable, sql string, b Budget) error {
	if err := b.validate(); err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin optimistic attempt: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path a rollback error
		// only means the connection died, and the server aborts the
		// transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	// INV: LK-2 — budgets are applied inside this transaction regardless of
	// the session defaults, so the attempt cannot outlive them even on a
	// misconfigured pool. A bare integer is milliseconds to PostgreSQL;
	// SET LOCAL cannot use bind parameters.
	setBudgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, setBudgets); err != nil {
		return fmt.Errorf("set attempt budgets: %w", err)
	}

	if _, err := tx.Exec(ctx, sql); err != nil {
		if budgetErr := asBudgetError(err, b); budgetErr != nil {
			return budgetErr
		}
		return fmt.Errorf("optimistic attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit optimistic attempt: %w", err)
	}
	return nil
}

// asBudgetError maps a PostgreSQL error to the budget it exceeded, or nil
// when the error is not a budget overrun.
func asBudgetError(err error, b Budget) *BudgetError {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	switch pgErr.Code {
	case sqlstateLockNotAvailable:
		return &BudgetError{Cause: CauseLock, Budget: b.LockTimeout}
	case sqlstateQueryCanceled:
		return &BudgetError{Cause: CauseStatement, Budget: b.StatementTimeout}
	default:
		return nil
	}
}
