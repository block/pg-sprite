package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
)

// TestAsConcurrentBudgetError covers the cancellation partition, in
// precedence order. A server 57014 at or past the overall budget is the
// budget's own statement_timeout whatever the caller's context did. Below
// that, the caller's own context ending is the caller's cancellation in
// either mode and in either form it arrives. Under a live context SQLSTATE
// 57014 is query_canceled generally, so an early one is an external
// cancellation and must not read as budget exhaustion, because a consumer
// branching on *BudgetError escalates to a heavier strategy, the wrong
// reaction to a deliberate operator cancel.
func TestAsConcurrentBudgetError(t *testing.T) {
	budget := ConcurrentBudget{Overall: time.Minute}
	callerOwned := ConcurrentBudget{CallerOwned: true}
	cancelled := &pgconn.PgError{Code: sqlstateQueryCanceled}
	live := t.Context()
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	assertNotBudgetOrExternal := func(t *testing.T, err error) {
		t.Helper()
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr), "a cancellation that is not the budget's must not read as budget exhaustion")
		assert.NotErrorIs(t, err, ErrCancelledExternally)
	}

	t.Run("57014 at or past the budget is budget exhaustion", func(t *testing.T) {
		err := asConcurrentBudgetError(live, cancelled, budget, time.Minute+time.Second, "concurrent index build")
		var budgetErr *BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, CauseStatement, budgetErr.Cause)
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server error must stay reachable behind the budget verdict")
		assert.Equal(t, sqlstateQueryCanceled, pgErr.Code)
	})

	// The budget's verdict outranks the caller's context: statement_timeout
	// fired at the configured limit, so the strategy was too slow whether or
	// not the caller also stopped waiting at the same instant. Reading it as
	// the caller's cancellation would hide the exhaustion the caller sized.
	t.Run("57014 at or past the budget under an ended context is still budget exhaustion", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, cancelled, budget, time.Minute+time.Second, "concurrent index build")
		var budgetErr *BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, CauseStatement, budgetErr.Cause)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
	})

	// Only the server's own 57014 can be the budget's: pgx giving up on the
	// statement client-side is the caller's cancellation at any elapsed.
	t.Run("the client's own context error at or past the budget is the caller's cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, context.DeadlineExceeded, budget, time.Minute+time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledByCaller)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assertNotBudgetOrExternal(t, err)
	})

	t.Run("57014 before the budget is an external cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(live, cancelled, budget, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr), "an external cancel must not read as budget exhaustion")
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server error must stay reachable for SQLSTATE branching")
		assert.Equal(t, sqlstateQueryCanceled, pgErr.Code)
	})

	t.Run("57014 under a live caller-owned context is an external cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(live, cancelled, callerOwned, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr))
	})

	t.Run("57014 under an ended caller-owned context is the caller's cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, cancelled, callerOwned, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledByCaller)
		assertNotBudgetOrExternal(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server error must stay reachable for SQLSTATE branching")
	})

	t.Run("57014 under an ended bounded context is the caller's cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, cancelled, budget, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledByCaller)
		assertNotBudgetOrExternal(t, err)
	})

	t.Run("the client's own context error under an ended context is the caller's cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, context.Canceled, callerOwned, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledByCaller)
		require.ErrorIs(t, err, context.Canceled, "the context error must stay reachable")
		assertNotBudgetOrExternal(t, err)
	})

	t.Run("a non-cancellation failure under an ended context is an operational failure", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, &pgconn.PgError{Code: "42P07"}, callerOwned, 2*time.Second, "concurrent index build")
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		assertNotBudgetOrExternal(t, err)
	})

	t.Run("any other failure is neither", func(t *testing.T) {
		err := asConcurrentBudgetError(live, &pgconn.PgError{Code: "42P07"}, budget, 2*time.Second, "concurrent index build")
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr))
	})
}

// TestIsStatementCancellationReadsEachFormOnItsOwn pins that the two forms
// a cancellation takes on the client are recognised independently: a
// server error of another kind sharing the chain with the client's own
// context error must not hide the context error, and a lone server error
// that is not query_canceled is not a cancellation.
func TestIsStatementCancellationReadsEachFormOnItsOwn(t *testing.T) {
	queryCanceled := &pgconn.PgError{Code: sqlstateQueryCanceled}
	adminShutdown := &pgconn.PgError{Code: "57P01"}
	connectionFailure := &pgconn.PgError{Code: "08006"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "server query_canceled", err: queryCanceled, want: true},
		{name: "wrapped server query_canceled", err: fmt.Errorf("exec: %w", queryCanceled), want: true},
		{name: "client context cancelled", err: context.Canceled, want: true},
		{name: "client deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "another server error ahead of the client's context error", err: fmt.Errorf("%w: %w", adminShutdown, context.Canceled), want: true},
		{name: "the client's context error ahead of another server error", err: fmt.Errorf("%w: %w", context.Canceled, adminShutdown), want: true},
		{name: "connection failure with the deadline", err: errors.Join(connectionFailure, context.DeadlineExceeded), want: true},
		{name: "another server error alone", err: adminShutdown, want: false},
		{name: "connection failure alone", err: connectionFailure, want: false},
		{name: "an untyped error", err: errors.New("boom"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isStatementCancellation(tt.err))
		})
	}
}

// cancelOnSecondRead is a clock that ends a context the second time it is
// read. The concurrent build reads its tracker's clock exactly twice — once
// before the statement, once the instant it returns — so the second read is
// the only point at which a caller can be made to cancel after a build has
// succeeded on the server and before its verdict runs.
type cancelOnSecondRead struct {
	cancel context.CancelFunc
	reads  int
}

// A pooler that drops lock_timeout and statement_timeout from the startup
// packet leaves the pool to apply them as statements on each new session
// instead. RESET restores what the startup packet carried, so on such an
// endpoint it restores nothing and the session goes back to the pool with
// both bounds at zero. The release must therefore put the bounds back by
// value: a reused session without them is the unbounded state LK-2 exists
// to prevent.
func TestBudgetedSessionRestoresBoundsTheStartupPacketNeverCarried(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(testutil.StartPostgres(t))
	require.NoError(t, err)
	// Absent from the startup packet and applied as statements below — the
	// arrangement this package's pool uses against a session-mode pooler
	// that drops them. Asserted rather than arranged: pgx seeds neither, so
	// deleting them would prove nothing, and a change that started seeding
	// them has to fail here rather than quietly leave the test testing the
	// packet it meant to strip.
	require.NotContains(t, cfg.ConnConfig.RuntimeParams, "lock_timeout")
	require.NotContains(t, cfg.ConnConfig.RuntimeParams, "statement_timeout")
	cfg.MaxConns = 1
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET lock_timeout = 3000; SET statement_timeout = 30000")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	conn, release, err := acquireBudgetedSession(t.Context(), pool, ConcurrentBudget{Overall: time.Minute})
	require.NoError(t, err)
	var buildPID, duringLock int64
	require.NoError(t, conn.QueryRow(t.Context(),
		`SELECT pg_backend_pid(), (SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'lock_timeout')`).
		Scan(&buildPID, &duringLock))
	require.Zero(t, duringLock, "the build itself runs with lock_timeout disabled")
	release()

	reused, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer reused.Release()
	var reusedPID, lockMS, statementMS int64
	require.NoError(t, reused.QueryRow(t.Context(), `SELECT
		pg_backend_pid(),
		(SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'lock_timeout'),
		(SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'statement_timeout')`).
		Scan(&reusedPID, &lockMS, &statementMS))
	require.Equal(t, buildPID, reusedPID,
		"the released session must be the one reused, or this proves nothing about it")
	assert.Equal(t, int64(3000), lockMS, "a reused session must not be left without a lock bound")
	assert.Equal(t, int64(30000), statementMS, "a reused session must not be left without a statement bound")
}
