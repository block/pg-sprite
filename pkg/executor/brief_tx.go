package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// catalogReadPath is the search_path every catalog read of the build runs
// under: pg_catalog alone. An index definition the server prints under it names
// the schema of every object it mentions, so a statement derived from the
// definition reads the same on any session, and a definition read here compares
// equal to one read anywhere else.
func catalogReadPath() []string { return []string{"pg_catalog"} }

// briefTx runs one bounded step of the build on a pooled session: a catalog
// read, the metadata-only parent index, an attach. Each runs in its own
// transaction under the pool's own lock and statement budgets.
type briefTx struct {
	pool *pgxpool.Pool
}

// run executes fn in a transaction whose search_path is path, and commits it, or
// always rolls it back when commit is false. A session that runs without a lock
// or statement budget is refused, because a statement that waits without limit
// holds the build hostage (LK-2).
func (b briefTx) run(ctx context.Context, what string, path []string, commit bool, fn func(context.Context, pgx.Tx) error) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", what, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns the
		// guaranteed ErrTxClosed, and on a failure path a rollback error only
		// means the connection died, which aborts the transaction anyway. It
		// runs detached and bounded so a cancelled run still reaches the
		// server and a wedged socket cannot hold the build.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	lockMS, statementMS, err := txBudgets(ctx, tx)
	if err != nil {
		return err
	}
	// INV: LK-2 — a baseline that disables either bound leaves the step
	// unbounded, so the step is refused before it runs.
	if lockMS == 0 || statementMS == 0 {
		return fmt.Errorf("%w: LK-2: the session runs with lock_timeout %dms and statement_timeout %dms, and a brief step needs both bounded",
			ErrInvariantViolation, lockMS, statementMS)
	}
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(path...)); err != nil {
		return fmt.Errorf("scope the search path of %s: %w", what, err)
	}
	started := time.Now()
	if err := fn(ctx, tx); err != nil {
		return asBriefStepError(ctx, err, what, lockMS, statementMS, time.Since(started))
	}
	if !commit {
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		// INV: LK-4 — a COMMIT that did not return success may still have
		// landed, so the step is never reported as rolled back.
		return &CommitOutcomeUnknownError{Err: fmt.Errorf("commit %s: %w", what, err)}
	}
	return nil
}

// read runs fn against the catalog under catalogReadPath in a transaction that
// always rolls back, so nothing fn runs persists. It does not open the
// transaction READ ONLY: the rollback, not the server, is what keeps the read
// from writing.
func (b briefTx) read(ctx context.Context, what string, fn func(context.Context, querier) error) error {
	return b.run(ctx, what, catalogReadPath(), false, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, tx)
	})
}

// txBudgetsSQL runs before the step's search_path is scoped, so every relation,
// operator, and cast in it is pg_catalog-qualified (CO-9).
const txBudgetsSQL = `SELECT
		(SELECT setting::pg_catalog.int8 FROM pg_catalog.pg_settings WHERE name OPERATOR(pg_catalog.=) 'lock_timeout'),
		(SELECT setting::pg_catalog.int8 FROM pg_catalog.pg_settings WHERE name OPERATOR(pg_catalog.=) 'statement_timeout')`

// txBudgets reads the lock and statement bounds in force on the transaction's
// session, in milliseconds.
func txBudgets(ctx context.Context, tx pgx.Tx) (lockMS, statementMS int64, err error) {
	if err := tx.QueryRow(ctx, txBudgetsSQL).Scan(&lockMS, &statementMS); err != nil {
		return 0, 0, fmt.Errorf("read session budgets: %w", err)
	}
	return lockMS, statementMS, nil
}

// asBriefStepError types a brief step's failure. A cancellation after the
// caller's context ended is the caller's own, whether the server's 57014 or
// the client's context error arrived first. An expired lock_timeout is the
// lock budget. SQLSTATE 57014 under a live context is query_canceled in
// general, which an operator's pg_cancel_backend also raises, so it is the
// statement budget only once the step has run at least that long:
// statement_timeout cannot fire before its budget elapses. An earlier one is
// an external cancellation, and anything else names the step.
func asBriefStepError(ctx context.Context, err error, what string, lockMS, statementMS int64, elapsed time.Duration) error {
	if ctxErr := ctx.Err(); ctxErr != nil && isStatementCancellation(err) {
		return fmt.Errorf("%w (%s, %w): %w", ErrCancelledByCaller, what, ctxErr, err)
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case sqlstateLockNotAvailable:
			return &BudgetError{Cause: CauseLock, Budget: time.Duration(lockMS) * time.Millisecond, cause: err}
		case sqlstateQueryCanceled:
			budget := time.Duration(statementMS) * time.Millisecond
			if elapsed >= budget {
				return &BudgetError{Cause: CauseStatement, Budget: budget, cause: err}
			}
			return fmt.Errorf("%w (%s, after %s of a %s statement budget): %w",
				ErrCancelledExternally, what, elapsed.Round(time.Millisecond), budget, err)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
