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

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

var (
	// ErrInvalidBlockingBudget means an accepted-blocking bound would be
	// disabled, cannot be represented by PostgreSQL, or is not longer than
	// the lock bound it is meant to outlast.
	ErrInvalidBlockingBudget = errors.New("invalid accepted-blocking budget")
	// ErrUnsupportedAcceptedBlocking means the statement is not one of the
	// single-relation blocking index forms this executor admits, the server
	// will not run it inside the engine-owned transaction, or it drops a
	// replica identity index. Retrying the same statement reproduces it;
	// the outcome is permanent.
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
	// INV: AB-1 — both limits are non-zero, representable, and ordered
	// before a session is acquired; whole milliseconds avoid PostgreSQL's
	// zero/off truncation.
	if b.LockTimeout < minBudget || b.LockTimeout > maxOverallBudget {
		return fmt.Errorf("%w: lock timeout must be between %s and %s, got %s", ErrInvalidBlockingBudget, minBudget, maxOverallBudget, b.LockTimeout)
	}
	if b.StatementTimeout < minBudget || b.StatementTimeout > maxOverallBudget {
		return fmt.Errorf("%w: statement timeout must be between %s and %s, got %s", ErrInvalidBlockingBudget, minBudget, maxOverallBudget, b.StatementTimeout)
	}
	// statement_timeout counts a lock wait too, so a statement budget that
	// is not longer than the lock budget would end every lock wait before
	// lock_timeout could: the pair is contradictory, the lock budget could
	// never be the operative bound, and the operator is asked for one that
	// can be.
	if b.StatementTimeout <= b.LockTimeout {
		return fmt.Errorf("%w: statement timeout must be longer than lock timeout, got statement %s and lock %s",
			ErrInvalidBlockingBudget, b.StatementTimeout, b.LockTimeout)
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
//
// table, when non-nil, is the schema-qualified table the statement locks,
// as the caller resolved it from the catalog. The executor requests that
// table's ACCESS EXCLUSIVE lock as a statement of its own before the DDL,
// so the whole lock wait happens where only the lock budget can end it and
// nothing has been submitted; the DDL's own lock requests are then granted
// at once and its statement budget measures the work. A nil table leaves
// the statement to acquire its locks itself — the caller's choice when the
// relation is one LOCK TABLE cannot name, such as a materialized view —
// and a statement cancellation then cannot distinguish a lock wait from
// work.
func ExecuteAcceptedBlocking(ctx context.Context, pool *pgxpool.Pool, sql string, table pgx.Identifier, b BlockingBudget) (BlockingReport, error) {
	return executeAcceptedBlocking(ctx, pool, sql, table, b, progress.WallClock{})
}

func executeAcceptedBlocking(ctx context.Context, pool *pgxpool.Pool, sql string, table pgx.Identifier, b BlockingBudget, clock progress.Clock) (rep BlockingReport, err error) {
	if err := b.validate(); err != nil {
		return rep, err
	}
	st, err := statement.ParseOne(sql)
	if err != nil {
		return rep, fmt.Errorf("%w: %w", ErrUnsupportedAcceptedBlocking, err)
	}
	if reason := acceptedBlockingIneligibility(st); reason != "" {
		return rep, fmt.Errorf("%w: %s", ErrUnsupportedAcceptedBlocking, reason)
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
	if table != nil {
		// INV: AB-2 — the acknowledged table's lock is requested on its own,
		// before the DDL, so every way the wait can end — lock_timeout, a
		// statement cancellation of this request, or the caller's context —
		// arrives while nothing has been submitted. The mode is ACCESS
		// EXCLUSIVE for every admitted form: it is the mode DROP INDEX takes
		// on the table, it is stronger than the SHARE REINDEX takes, and
		// holding it means no other session holds a conflicting lock on any
		// index of the table, so the DDL's own requests are granted at once.
		// Without ONLY the request recurses to a partitioned table's
		// partitions, which the DDL would also lock in turn.
		lock := "LOCK TABLE " + table.Sanitize() + " IN ACCESS EXCLUSIVE MODE"
		if _, err := tx.Exec(ctx, lock); err != nil {
			rep.Duration = clock.Now().Sub(start)
			return rep, acceptedBlockingLockError(ctx, err, b)
		}
	}
	if st.Kind() == statement.KindDropIndex {
		if err := refuseReplicaIdentityDrop(ctx, tx, st.IndexRelation()); err != nil {
			rep.Duration = clock.Now().Sub(start)
			return rep, err
		}
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

// refuseReplicaIdentityDrop refuses a DROP INDEX that would drop a replica
// identity index: the named index itself, or one attached under it on a
// partition. PostgreSQL keeps REPLICA IDENTITY USING INDEX when the index
// goes, so the table is left with no usable identity and every UPDATE and
// DELETE on it fails while a publication publishes them. It runs under the
// table's ACCESS EXCLUSIVE lock, which ALTER TABLE ... REPLICA IDENTITY also
// needs, so the answer cannot change before the DROP runs. The index is
// counted on its own as well as through pg_partition_tree, which lists
// nothing for an index outside a partition tree. A name that resolves to
// nothing has no identity to protect; the DROP reports it.
func refuseReplicaIdentityDrop(ctx context.Context, tx pgx.Tx, rel statement.IndexRelation) error {
	name := pgx.Identifier{rel.Name}
	if rel.Schema != "" {
		name = pgx.Identifier{rel.Schema, rel.Name}
	}
	var identity string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(string_agg(format('%s on %s', i.indexrelid::regclass, i.indrelid::regclass), ', '
		                           ORDER BY i.indexrelid::regclass::text), '')
		FROM (SELECT to_regclass($1)::oid AS relid
		      UNION
		      SELECT relid FROM pg_partition_tree(to_regclass($1))) tree
		JOIN pg_index i ON i.indexrelid = tree.relid AND i.indisreplident
		JOIN pg_class c ON c.oid = i.indrelid AND c.relreplident = 'i'`, name.Sanitize()).Scan(&identity)
	if err != nil {
		return fmt.Errorf("check whether %s is a replica identity index: %w", name.Sanitize(), err)
	}
	if identity != "" {
		return fmt.Errorf("%w: dropping %s drops the replica identity index %s, which makes UPDATE and DELETE fail "+
			"while a publication publishes them; point REPLICA IDENTITY at another unique index first (or DEFAULT, if a usable primary key remains after the drop)",
			ErrUnsupportedAcceptedBlocking, name.Sanitize(), identity)
	}
	return nil
}

// acceptedBlockingIneligibility returns why a statement is outside the
// accepted-blocking shape, or "" when it is eligible.
func acceptedBlockingIneligibility(st statement.Statement) string {
	if st.Concurrent() {
		return "the concurrent form cannot run inside the engine-owned transaction; run it directly against the database"
	}
	switch st.Kind() {
	case statement.KindCreateIndex:
		return ""
	case statement.KindDropIndex, statement.KindReindex:
		// INV: AB-3 — only a statement whose whole scope is the one
		// acknowledged table runs here; a DROP INDEX ... CASCADE also drops
		// dependent objects on other tables and parses as another target.
		return indexScopeIneligibility(st)
	default:
		return "it is not an index statement"
	}
}

// indexScopeIneligibility names why a DROP INDEX or REINDEX statement's scope
// is wider than the one acknowledged table, or returns "" when it is not.
func indexScopeIneligibility(st statement.Statement) string {
	if st.IndexTarget() == statement.IndexTargetSingleRelation {
		return ""
	}
	if st.Cascades() {
		return "a cascading drop also drops the objects that depend on the index, which the acknowledged table's lock does not cover; " +
			"resubmit without CASCADE, and PostgreSQL then refuses the drop if anything depends on the index"
	}
	if st.Kind() == statement.KindDropIndex {
		return "it names several indexes; submit one DROP INDEX per index"
	}
	return "its scope is wider than one relation; name one index or table"
}

// acceptedBlockingLockError maps a failed table-lock request. The DDL has
// not been submitted, so no outcome here is unknown: a lock_timeout or a
// statement cancellation both mean the lock was not granted within the
// budgets (the cancellation is reachable only when the request recurses to
// partitions, so the budget it names is the one that cut the request off),
// the caller's context ending is the caller's own cancellation of a wait,
// and anything else is the server refusing the request itself.
func acceptedBlockingLockError(ctx context.Context, err error, b BlockingBudget) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: the accepted table lock was still being waited for, so nothing was submitted: %w",
			ErrCancelledByCaller, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlstateLockNotAvailable:
			// INV: AB-2 — the lock request was refused; the DDL never ran.
			return &BudgetError{Cause: CauseLock, Budget: b.LockTimeout, Attempts: 1, cause: err}
		case sqlstateQueryCanceled:
			return &BudgetError{Cause: CauseLock, Budget: b.StatementTimeout, Attempts: 1, cause: err}
		}
	}
	return &BlockingExecutionError{Err: fmt.Errorf("lock the accepted table: %w", err)}
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
			// With the table lock already held the statement's bound ended
			// work the operator accepted; only a statement that acquired its
			// own locks could have been cut off while still waiting.
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
