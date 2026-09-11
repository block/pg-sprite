package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

var (
	// ErrInvalidBlockingBudget means an accepted-blocking bound would be
	// disabled or cannot be represented by PostgreSQL.
	ErrInvalidBlockingBudget = errors.New("invalid accepted-blocking budget")
	// ErrUnsupportedAcceptedBlocking means the statement is not one of the
	// single-relation blocking index forms this executor admits, or the
	// server will not run it inside the engine-owned transaction. Retrying
	// the same statement reproduces it; the outcome is permanent.
	ErrUnsupportedAcceptedBlocking = errors.New("statement is not an accepted blocking index statement")
)

// sqlstateActiveSQLTransaction is raised when a statement that must own its
// transaction is submitted inside a transaction block. In the admitted set
// only REINDEX on a partitioned table or index does this: PostgreSQL
// reindexes each partition in its own transaction and refuses before
// touching any of them.
const sqlstateActiveSQLTransaction = "25001"

// BlockingBudget bounds one operator-accepted blocking statement.
type BlockingBudget struct {
	LockTimeout      time.Duration
	StatementTimeout time.Duration
}

func (b BlockingBudget) validate() error {
	// INV: AB-1 — both limits are non-zero and representable before a
	// session is acquired; whole milliseconds avoid PostgreSQL's zero/off
	// truncation.
	if b.LockTimeout < minBudget || b.LockTimeout > maxOverallBudget {
		return fmt.Errorf("%w: lock timeout must be between %s and %s, got %s", ErrInvalidBlockingBudget, minBudget, maxOverallBudget, b.LockTimeout)
	}
	if b.StatementTimeout < minBudget || b.StatementTimeout > maxOverallBudget {
		return fmt.Errorf("%w: statement timeout must be between %s and %s, got %s", ErrInvalidBlockingBudget, minBudget, maxOverallBudget, b.StatementTimeout)
	}
	return nil
}

// BlockingReport records a committed accepted-blocking statement and its
// engine-owned bounds.
type BlockingReport struct {
	SQL              string        `json:"sql"`
	LockTimeout      time.Duration `json:"lock_timeout_ns"`
	StatementTimeout time.Duration `json:"statement_timeout_ns"`
	Duration         time.Duration `json:"duration_ns"`
}

// BlockingExecutionError reports a PostgreSQL failure after the accepted
// statement was submitted. Err retains the server SQLSTATE.
type BlockingExecutionError struct{ Err error }

func (e *BlockingExecutionError) Error() string {
	return fmt.Sprintf("accepted blocking statement failed: %v", e.Err)
}
func (e *BlockingExecutionError) Unwrap() error { return e.Err }

// BlockingOutcomeUnknownError means the client cannot establish whether
// the transaction committed; the catalog must be inspected before retrying.
type BlockingOutcomeUnknownError struct{ Err error }

func (e *BlockingOutcomeUnknownError) Error() string {
	return fmt.Sprintf("accepted blocking statement outcome is unknown until the catalog is inspected: %v", e.Err)
}
func (e *BlockingOutcomeUnknownError) Unwrap() error { return e.Err }

// ExecuteAcceptedBlocking runs one admitted blocking index statement in one
// engine-owned transaction with explicit lock and statement bounds.
func ExecuteAcceptedBlocking(ctx context.Context, pool *pgxpool.Pool, sql string, b BlockingBudget) (BlockingReport, error) {
	return executeAcceptedBlocking(ctx, pool, sql, b, progress.WallClock{})
}

func executeAcceptedBlocking(ctx context.Context, pool *pgxpool.Pool, sql string, b BlockingBudget, clock progress.Clock) (rep BlockingReport, err error) {
	if err := b.validate(); err != nil {
		return rep, err
	}
	st, err := statement.ParseOne(sql)
	if err != nil {
		return rep, fmt.Errorf("%w: %w", ErrUnsupportedAcceptedBlocking, err)
	}
	if !acceptedBlockingShape(st) {
		return rep, ErrUnsupportedAcceptedBlocking
	}
	rep = BlockingReport{SQL: sql, LockTimeout: b.LockTimeout, StatementTimeout: b.StatementTimeout}
	start := clock.Now()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return BlockingReport{}, fmt.Errorf("begin accepted blocking statement: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// INV: AB-1 — transaction-local values override every caller/session
	// default on the same engine-owned session that executes the statement.
	settings := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, settings); err != nil {
		return BlockingReport{}, fmt.Errorf("set accepted blocking budgets: %w", err)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		rep.Duration = clock.Now().Sub(start)
		return rep, acceptedBlockingStatementError(ctx, err, b)
	}
	if err := tx.Commit(ctx); err != nil {
		rep.Duration = clock.Now().Sub(start)
		return rep, &BlockingOutcomeUnknownError{Err: err}
	}
	rep.Duration = clock.Now().Sub(start)
	return rep, nil
}

func acceptedBlockingShape(st statement.Statement) bool {
	if st.Concurrent() {
		return false
	}
	switch st.Kind() {
	case statement.KindCreateIndex:
		return true
	case statement.KindDropIndex, statement.KindReindex:
		return st.IndexTarget() == statement.IndexTargetSingleRelation
	default:
		return false
	}
}

func acceptedBlockingStatementError(ctx context.Context, err error, b BlockingBudget) error {
	if ctx.Err() != nil {
		return &BlockingOutcomeUnknownError{Err: err}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlstateLockNotAvailable:
			// INV: AB-2 — PostgreSQL rejected lock acquisition and aborted the
			// transaction before the submitted DDL could execute.
			return &BudgetError{Cause: CauseLock, Budget: b.LockTimeout, Attempts: 1, cause: err}
		case sqlstateQueryCanceled:
			return &BudgetError{Cause: CauseStatement, Budget: b.StatementTimeout, Attempts: 1, cause: err}
		case sqlstateActiveSQLTransaction:
			// The server refused the statement before it executed, and it
			// will refuse it every time: the engine-owned transaction is the
			// only way this executor runs anything. Report the statement as
			// outside the path rather than as a retryable execution failure.
			return fmt.Errorf("%w: the server will not run it inside the engine-owned transaction: %w",
				ErrUnsupportedAcceptedBlocking, err)
		default:
			return &BlockingExecutionError{Err: err}
		}
	}
	return &BlockingExecutionError{Err: err}
}
