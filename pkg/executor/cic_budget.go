package executor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// buildMinConns is the pool size a concurrent build needs: the build
// session and the verdict session reserved beside it.
const buildMinConns = 2

// recoveryMinConns is the pool size a rebuild recovery needs: its own
// session, held across the drops and the build, plus the build's own two.
const recoveryMinConns = buildMinConns + 1

// dropRecoveryMinConns is the pool size a drop-only recovery needs: its own
// session, held across the drops, and the drop session beside it. It is
// its own constant because it counts different sessions from buildMinConns
// and only happens to equal it.
const dropRecoveryMinConns = 2

// sessionCleanupTimeout bounds the client-side session housekeeping around
// a build: resetting the budget overrides and closing a session that must
// be discarded. It is deliberately separate from the build budget — a
// wedged socket must not hang the executor for the whole build budget.
const sessionCleanupTimeout = 5 * time.Second

// verdictTimeout bounds the post-failure catalog verdict: a backend-stop
// poll and one catalog snapshot, on a connection reserved before the build
// started. It is deliberately independent of the build budget — the
// verdict's work does not scale with the build's, so a build that fails in
// milliseconds must not hold its caller for an hours-long budget. The cost
// of the bound is one rare shape: a backend that outlives its client
// failure longer than this (a lost cancel signal) resolves indeterminate —
// fail-closed — instead of eventually proven.
const verdictTimeout = 30 * time.Second

// ConcurrentBudget bounds one CONCURRENTLY statement (index build or drop).
//
// CONCURRENTLY statements get their own wait policy instead of the blanket
// per-lock timeout: their waits for other transactions' snapshots are lock
// waits by implementation, so a session lock_timeout would cancel a healthy
// build mid-wait — and that cancellation is exactly what creates the invalid
// index this executor exists to prevent. The statement therefore runs with
// lock_timeout disabled and one bound on the whole statement: a server
// deadline, or in caller-owned mode the caller's cancellable context. That
// is safe with respect to the lock queue: the SHARE UPDATE EXCLUSIVE lock a
// concurrent build waits for does not block normal reads or writes queued
// behind it.
type ConcurrentBudget struct {
	// Overall bounds the whole statement, waits included, via
	// statement_timeout. It must be at least one millisecond (PostgreSQL's
	// granularity); expect index builds on large tables to need a generous
	// value.
	Overall time.Duration
	// CallerOwned makes the caller's cancellable context the statement's only
	// bound: the session runs with statement_timeout disabled and Overall must
	// be zero. The executor refuses a context that cannot be cancelled.
	CallerOwned bool
}

// maxOverallBudget is PostgreSQL's ceiling for statement_timeout (the
// setting is a signed 32-bit millisecond count); a larger value would be
// rejected — or worse, mis-set — by the server, leaving the statement
// unbounded.
const maxOverallBudget = time.Duration(math.MaxInt32) * time.Millisecond

// validate rejects budgets that would leave the statement unbounded.
func (b ConcurrentBudget) validate() error {
	if b.CallerOwned {
		// INV: LK-2 — the bound moves from the server timer to the caller's
		// cancellable context, checked before a session is acquired.
		if b.Overall != 0 {
			return fmt.Errorf("%w: got %s", ErrCallerOwnedOverallBudget, b.Overall)
		}
		return nil
	}
	// INV: LK-2 — the build is bounded by construction; below one
	// millisecond the setting would round to zero, which disables
	// statement_timeout entirely.
	if b.Overall < time.Millisecond {
		return fmt.Errorf("%w: overall budget must be at least 1ms, got %s", ErrUnboundedBudget, b.Overall)
	}
	if b.Overall > maxOverallBudget {
		return fmt.Errorf("%w: overall budget must be at most %s, got %s", ErrUnboundedBudget, maxOverallBudget, b.Overall)
	}
	return nil
}

// acquireBudgetedSession acquires one pooled session and applies the
// CONCURRENTLY wait policy to it. CONCURRENTLY statements refuse to run
// inside a transaction block, which also rules out SET LOCAL, so the
// overrides are session-level: the returned release resets them before the
// session goes back to the pool and discards the session when the reset
// cannot be proven.
func acquireBudgetedSession(ctx context.Context, pool *pgxpool.Pool, b ConcurrentBudget) (*pgxpool.Conn, func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire session: %w", err)
	}

	// The bounds in force are read before they are overridden so the release
	// can put them back as they were. Nothing has been changed yet, so a
	// session whose read fails is still exactly as the pool prepared it and
	// goes back unharmed.
	priorLock, priorStatement, err := sessionBudgets(ctx, conn)
	if err != nil {
		conn.Release()
		return nil, nil, err
	}

	// INV: LK-2 — the CONCURRENTLY exception policy: no per-lock timeout
	// (a lock_timeout would cancel the statement's snapshot waits and leave
	// the invalid index this executor exists to prevent); either one overall
	// statement deadline or caller cancellation bounds the build instead. A bare integer is
	// milliseconds to PostgreSQL; the settings are applied here regardless
	// of the pool's defaults.
	statementTimeout := b.Overall.Milliseconds()
	if b.CallerOwned {
		statementTimeout = 0
	}
	budgets := "SET lock_timeout = 0; SET statement_timeout = " + strconv.FormatInt(statementTimeout, 10)
	if _, err := conn.Exec(ctx, budgets); err != nil {
		// The two SETs may have partially applied — PostgreSQL runs a
		// simple-query batch statement by statement — so the session must
		// not return to the pool: releasing it would hand lock_timeout = 0
		// to an unsuspecting borrower. Hijacking detaches it from the pool
		// structurally; the close error only restates that the connection
		// is already unusable, and the Exec failure is the one returned.
		discardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		_ = conn.Hijack().Close(discardCtx)
		return nil, nil, fmt.Errorf("set session budgets: %w", err)
	}

	release := func() {
		// Housekeeping runs even when ctx is cancelled — a cancelled build
		// still used this session — under its own short bound: a wedged
		// socket must not hang the executor.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		// The bounds are put back by value rather than with RESET. RESET
		// restores what the startup packet carried, which is not where the
		// bounds necessarily came from: a pooler that drops those parameters
		// leaves the pool to apply them as statements instead, and a RESET
		// there would hand back a session with both bounds at zero — the
		// unbounded state LK-2 exists to prevent, on a connection the pool
		// then reuses.
		restore := "SET lock_timeout = " + strconv.FormatInt(priorLock, 10) +
			"; SET statement_timeout = " + strconv.FormatInt(priorStatement, 10)
		if _, err := conn.Exec(cleanupCtx, restore); err == nil {
			conn.Release()
			return
		}
		// The reset could not be proven, so the session must not be
		// reused: hijacking detaches it from the pool structurally, and
		// the close error only means the connection is already unusable.
		_ = conn.Hijack().Close(cleanupCtx)
	}
	return conn, release, nil
}

// sessionBudgets reads the execution bounds in force on conn, in the
// milliseconds a bare integer SET writes.
//
// pg_settings reports both in milliseconds, where current_setting renders
// them as text under PostgreSQL's unit rules — "3s" for 3000 and "500ms"
// for 500 — which a caller that means to write the value straight back
// would have to parse.
func sessionBudgets(ctx context.Context, conn *pgxpool.Conn) (lockMS, statementMS int64, err error) {
	const query = `SELECT
		(SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'lock_timeout'),
		(SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'statement_timeout')`
	if err := conn.QueryRow(ctx, query).Scan(&lockMS, &statementMS); err != nil {
		return 0, 0, fmt.Errorf("read session budgets: %w", err)
	}
	return lockMS, statementMS, nil
}

// asConcurrentBudgetError types a CONCURRENTLY statement's failure into
// the three cancellation causes a consumer branches on — the budget, the
// caller, or a third party — or wraps anything else as an operational
// failure, in which the operation names the statement that failed.
//
// The budget is checked first. SQLSTATE 57014 is query_canceled generally,
// not statement_timeout specifically, but the budget's own
// statement_timeout cannot fire before the budget elapses, so a 57014 at
// or past the overall budget is budget exhaustion (*BudgetError) — even
// when the caller's context has also ended, because an orchestrator's
// deadline commonly sits just outside the budget it configured, and the
// escalation signal a *BudgetError carries must not be lost to that
// coincidence; the caller can always read its own ctx.Err(). The boundary
// is approximate by network latency — a cancel landing within that sliver
// of the deadline reads as the budget — but the two truths coincide there.
//
// Below the budget the caller's context is the exact signal: when it has
// ended, the cancellation is the caller's own (ErrCancelledByCaller),
// whether it reached the executor as the server's 57014 or as the client's
// context error — which of the two arrives first is a race the caller
// cannot observe, so both are typed the same way. A 57014 under a live
// context and before the budget is then neither the budget's nor the
// caller's — an operator's pg_cancel_backend raises the same code — and
// surfaces as ErrCancelledExternally. In caller-owned mode there is no
// server deadline, so every 57014 under a live context is external.
func asConcurrentBudgetError(ctx context.Context, err error, b ConcurrentBudget, elapsed time.Duration, operation string) error {
	var pgErr *pgconn.PgError
	serverCancelled := errors.As(err, &pgErr) && pgErr.Code == sqlstateQueryCanceled
	if serverCancelled && !b.CallerOwned && elapsed >= b.Overall {
		return &BudgetError{Cause: CauseStatement, Budget: b.Overall, cause: err}
	}
	if ctx.Err() != nil && isStatementCancellation(err) {
		return fmt.Errorf("%w (after %s, %w): %w",
			ErrCancelledByCaller, elapsed.Round(time.Millisecond), ctx.Err(), err)
	}
	if serverCancelled {
		if b.CallerOwned {
			return fmt.Errorf("%w (after %s, caller-owned): %w",
				ErrCancelledExternally, elapsed.Round(time.Millisecond), err)
		}
		return fmt.Errorf("%w (after %s of a %s budget): %w",
			ErrCancelledExternally, elapsed.Round(time.Millisecond), b.Overall, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// isStatementCancellation reports whether err is a cancelled statement in
// either of the forms a cancellation takes on the client: the server's
// query_canceled, or the client's own context error when pgx gave up on
// the statement before the server's response arrived. Each form is checked
// on its own, so a server error of another kind in the same chain does not
// hide the client's context error behind it.
func isStatementCancellation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateQueryCanceled {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
