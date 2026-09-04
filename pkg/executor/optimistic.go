// Package executor runs schema changes against the database. It holds the
// optimistic front door — attempt the change directly under a tight
// lock_timeout and statement_timeout so it can only succeed if it is
// effectively an instant / in-place change; a budget overrun cancels the
// statement cleanly, nothing is executed, and a typed BudgetError surfaces
// for the caller to turn into a not-native-safe verdict — and the native
// executors for the classified safe idioms, starting with the concurrent
// index build (see native.go).
//
// This is a safety-critical core package: see SAFETY.md. It never trusts the
// caller's classification — its own protections are the budget, applied with
// SET LOCAL inside the attempt's transaction regardless of session defaults,
// and the statement binding: it accepts only a parsed statement.Statement
// (exactly one statement by construction) whose target matches the
// preflighted table (invariant ST-7).
package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrInvariantViolation is the fail-closed error class for breaches of the
// registry in docs/invariants.md (see SAFETY.md); the message names the
// invariant ID. It is never a warning and never retried.
var ErrInvariantViolation = errors.New("invariant violation")

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

// BudgetError reports that an execution attempt exceeded one of its budgets
// and was cancelled cleanly by the server: the statement did not take
// effect. It is a refusal input, not an operational failure.
type BudgetError struct {
	// Cause is the budget that was exceeded.
	Cause BudgetCause
	// Budget is the configured limit for that cause.
	Budget time.Duration
	// Attempts is the number of bounded transactions tried. It is greater
	// than one when lock acquisition retries were exhausted.
	Attempts int
}

// Error implements the error interface.
func (e *BudgetError) Error() string {
	if e.Attempts > 1 {
		return fmt.Sprintf("execution exceeded its %s (%s) after %d bounded attempts", e.Cause, e.Budget, e.Attempts)
	}
	return fmt.Sprintf("execution exceeded its %s (%s) and was cancelled", e.Cause, e.Budget)
}

// RetryPolicy bounds retries after lock_timeout expires. Backoff doubles
// after each failed attempt and is capped at MaxBackoff.
type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// DefaultRetryPolicy returns the safe native-DDL retry policy used by
// callers that do not need to tune lock acquisition.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     time.Second,
	}
}

func (p RetryPolicy) validate() error {
	if p.MaxAttempts < 1 {
		return fmt.Errorf("retry attempts must be at least 1, got %d", p.MaxAttempts)
	}
	if p.InitialBackoff < 0 {
		return fmt.Errorf("initial retry backoff must not be negative, got %s", p.InitialBackoff)
	}
	// Zero backoff with retries enabled would re-enter the lock queue
	// back-to-back, each occupancy blocking queued traffic for the full
	// lock budget with no pause for the blocker to drain.
	if p.MaxAttempts > 1 && p.InitialBackoff == 0 {
		return fmt.Errorf("initial retry backoff must be positive when retries are enabled, got %d attempts with no backoff", p.MaxAttempts)
	}
	if p.MaxBackoff < p.InitialBackoff {
		return fmt.Errorf("maximum retry backoff %s must be at least initial backoff %s", p.MaxBackoff, p.InitialBackoff)
	}
	return nil
}

// Budget bounds one optimistic attempt. Both limits must be at least
// minBudget: an unbounded attempt is exactly the stall the front door exists
// to prevent.
type Budget struct {
	// LockTimeout bounds how long the attempt may wait in the lock queue.
	LockTimeout time.Duration
	// StatementTimeout bounds how long the statement may run once started.
	StatementTimeout time.Duration
}

// minBudget is the smallest expressible budget: budgets are applied to
// PostgreSQL in whole milliseconds, and a sub-millisecond value would
// truncate to 0 — which disables the corresponding limit entirely.
const minBudget = time.Millisecond

// validate rejects budgets that would leave the attempt unbounded. Rejecting
// a sub-millisecond budget is more fail-closed than silently rounding it up.
func (b Budget) validate() error {
	// INV: LK-2 — the attempt is bounded by construction; a zero, negative,
	// or sub-millisecond timeout would disable the corresponding PostgreSQL
	// limit after millisecond truncation.
	if b.LockTimeout < minBudget {
		return fmt.Errorf("lock budget must be at least %s, got %s", minBudget, b.LockTimeout)
	}
	if b.StatementTimeout < minBudget {
		return fmt.Errorf("statement budget must be at least %s, got %s", minBudget, b.StatementTimeout)
	}
	// Both settings are int32-millisecond server GUCs sent raw via
	// SET LOCAL: a value beyond the server ceiling would be rejected
	// mid-attempt as an out-of-range setting — an operational error where
	// a budget defect decidable here should refuse at admission.
	if b.LockTimeout > maxOverallBudget {
		return fmt.Errorf("lock budget must be at most %s, got %s", maxOverallBudget, b.LockTimeout)
	}
	if b.StatementTimeout > maxOverallBudget {
		return fmt.Errorf("statement budget must be at most %s, got %s", maxOverallBudget, b.StatementTimeout)
	}
	return nil
}

// ExecuteNative runs transactional native DDL under transaction-local
// budgets and retries only lock_timeout failures, bounded by retry. The
// table must have passed preflight and the statement must target it — both
// proofs make the unsafe call unrepresentable: a statement.Statement can
// only come from ParseOne (exactly one statement, parsed by the real
// grammar), and a target mismatch is refused before anything executes, so a
// proof for one table cannot smuggle SQL against another. Each attempt is a
// new transaction, so neither an aborted transaction nor its settings can
// leak through the pool. When the proof carries a schema, each attempt runs
// with search_path pinned to that schema then public, so the statement's
// unqualified secondary names resolve in the target schema — the same
// resolution the create path and the introspection read path use. On
// success the change is committed: it was effectively instant. If the lock
// budget is exhausted across all bounded attempts, a *BudgetError carrying
// the attempt count is returned.
// Statement timeouts and all other failures return immediately: repeating
// work that exceeded its execution budget is not a lock-acquisition
// strategy.
func ExecuteNative(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, st statement.Statement, b Budget, retry RetryPolicy) error {
	return executeNative(ctx, pool, pt, st, b, retry, nil)
}

// ExecuteNativeWithProgress runs an optimistic native attempt while updating
// tracker. The caller may poll tracker concurrently with this blocking call.
func ExecuteNativeWithProgress(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, st statement.Statement, b Budget, retry RetryPolicy, tracker *progress.Tracker) (err error) {
	if tracker == nil {
		return fmt.Errorf("%w: progress tracker is required", ErrInvariantViolation)
	}
	tracker.Start(1, progress.OperationOptimistic)
	tracker.StartStep(1, progress.OperationOptimistic, st.SQL())
	defer func() { tracker.Finish(err) }()
	return executeNative(ctx, pool, pt, st, b, retry, tracker)
}

func executeNative(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, st statement.Statement, b Budget, retry RetryPolicy, tracker *progress.Tracker) error {
	if err := b.validate(); err != nil {
		return err
	}
	if err := retry.validate(); err != nil {
		return err
	}
	// INV: ST-7 — the executor runs exactly the statement that was gated,
	// and only against the table the preflight proof verified.
	if st.Table() == "" || st.Schema() != pt.Schema() || st.Table() != pt.Table() {
		return fmt.Errorf("%w: ST-7: statement targets %q but preflight verified %q",
			ErrInvariantViolation, qualifiedName(st.Schema(), st.Table()), qualifiedName(pt.Schema(), pt.Table()))
	}
	// The attempt runs with search_path pinned to the proof's schema (when
	// the proof carries one — an unqualified lookup carries none and runs
	// under the session default), so the statement's unqualified secondary
	// names — a column's type, an expression's function — resolve in the
	// target schema whether the run creates the table or alters it.
	return executeWithLockRetryObserved(ctx, retry, func(ctx context.Context) error {
		return executeBoundedAttempt(ctx, pool, st, b, pt.Schema())
	}, sleepContext, func(attempt int) {
		if tracker != nil {
			tracker.SetAttempt(attempt)
		}
	})
}

// executeBoundedAttempt is the shared transactional attempt behind the
// optimistic and create paths. When searchPathSchema is set, the
// transaction's search_path is pinned to that schema then public — the
// same policy the introspection read path sets — so a statement's
// unqualified references (a column's type, an expression's function)
// resolve exactly as the diff resolved them, never via the session's
// ambient search_path.
func executeBoundedAttempt(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, b Budget, searchPathSchema string) error {
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
	// SET LOCAL cannot use bind parameters, and identifiers are sanitized.
	setBudgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if searchPathSchema != "" {
		setBudgets += "; SET LOCAL search_path = " + pgx.Identifier{searchPathSchema}.Sanitize() + ", public"
	}
	if _, err := tx.Exec(ctx, setBudgets); err != nil {
		return fmt.Errorf("set attempt budgets: %w", err)
	}

	if _, err := tx.Exec(ctx, st.SQL()); err != nil {
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

type sleepFunc func(context.Context, time.Duration) error

func executeWithLockRetry(ctx context.Context, policy RetryPolicy, attempt func(context.Context) error, sleep sleepFunc) error {
	return executeWithLockRetryObserved(ctx, policy, attempt, sleep, func(int) {})
}

func executeWithLockRetryObserved(ctx context.Context, policy RetryPolicy, attempt func(context.Context) error, sleep sleepFunc, observe func(int)) error {
	for i := 1; i <= policy.MaxAttempts; i++ {
		observe(i)
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		var budgetErr *BudgetError
		if !errors.As(err, &budgetErr) || budgetErr.Cause != CauseLock {
			return err
		}
		if i == policy.MaxAttempts {
			budgetErr.Attempts = i
			return budgetErr
		}
		if err := sleep(ctx, retryBackoff(policy, i)); err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: LK-2: retry loop escaped its bounded attempts", ErrInvariantViolation)
}

func retryBackoff(policy RetryPolicy, failedAttempts int) time.Duration {
	delay := policy.InitialBackoff
	for i := 1; i < failedAttempts && delay < policy.MaxBackoff; i++ {
		if delay > policy.MaxBackoff/2 {
			return policy.MaxBackoff
		}
		delay *= 2
	}
	if delay > policy.MaxBackoff {
		return policy.MaxBackoff
	}
	return delay
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// qualifiedName renders schema.table for error messages, omitting the dot
// when the name is unqualified.
func qualifiedName(schema, table string) string {
	if schema == "" {
		return table
	}
	return schema + "." + table
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
