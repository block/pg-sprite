package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// ErrDroppedTableHasParent means a reviewed table drop names a partition or
// an inheritance child, read under the table's lock. Its rows are also
// rows of the table above it, so dropping it changes what that table holds,
// which the plan did not review. Nothing was dropped. The outcome is
// permanent.
var ErrDroppedTableHasParent = errors.New("the table the plan drops is a partition or an inheritance child")

// ErrDroppedTableHasDependents means PostgreSQL refused the reviewed table
// drop because another object depends on the table, such as a view or a
// foreign key of another table. The drop never says CASCADE, so the
// server refuses rather than remove objects the plan did not review.
// Nothing was dropped. The outcome is permanent until the dependent
// objects are removed or changed.
var ErrDroppedTableHasDependents = errors.New("other objects depend on the table the plan drops")

// sqlstateDependentObjectsStillExist is the server's refusal of a drop
// without CASCADE that other objects depend on.
const sqlstateDependentObjectsStillExist = "2BP01"

// sqlstateInvalidSchemaName is the server's refusal of a name in a schema
// that does not exist.
const sqlstateInvalidSchemaName = "3F000"

// sqlstateWrongObjectType is the server's refusal of a statement on a
// relation of the wrong kind, such as LOCK TABLE on a sequence.
const sqlstateWrongObjectType = "42809"

// ExecuteDropTable drops one reviewed table with DROP TABLE, without
// CASCADE, in a single transaction under the budget b, retrying a lock
// timeout under retry. A caller has already admitted the drop as
// destructive; definition is the one its plan recorded with
// schemadiff.DroppedTableDefinition.
//
// Each attempt takes the table's ACCESS EXCLUSIVE lock, which the drop
// takes anyway, before reading anything, and holds it until the drop
// commits, so the table cannot change between the check and the drop. Under
// the lock it reads the table again and refuses, dropping nothing:
//
//   - when the table no longer exists (ErrTableNotFound);
//   - when its definition differs from the reviewed one, or the name now
//     stands for another kind of relation (ErrDroppedObjectDrift);
//   - when it is a partitioned table or has inheritance children, which
//     the drop would remove too (ErrDroppedObjectHasDescendants);
//   - when it is a partition or an inheritance child
//     (ErrDroppedTableHasParent).
//
// A view, another table's foreign key, or any other object that depends on
// the table makes the server refuse the drop (ErrDroppedTableHasDependents).
// A COMMIT that does not return success reports *CommitOutcomeUnknownError:
// the table may be gone, so the caller inspects the catalog before retrying.
func ExecuteDropTable(ctx context.Context, pool *pgxpool.Pool, schema, table, definition string, b Budget, retry RetryPolicy) error {
	if err := b.validate(); err != nil {
		return fmt.Errorf("drop table: %w", err)
	}
	if err := retry.validate(); err != nil {
		return fmt.Errorf("drop table: %w", err)
	}
	if schema == "" || table == "" {
		return fmt.Errorf("%w: a table drop needs a schema and a table, got %q.%q", ErrInvariantViolation, schema, table)
	}
	if definition == "" {
		return fmt.Errorf("%w: a table drop needs the definition its plan reviewed", ErrInvariantViolation)
	}
	// INV: LK-2 — the drop takes ACCESS EXCLUSIVE on the table and its
	// descendants, so it requests the lock once no long-lived reader or
	// writer holds one there, and ends without requesting it when one is
	// still there after HolderWait.
	held := []lockedTable{{schema: schema, table: table, descendants: true, modes: readAndWriteLockModes()}}
	if err := awaitLockQueue(ctx, pool, held, retry.HolderWait, systemHolderWaitClock(), trackerHolderWaitReport(nil)); err != nil {
		return fmt.Errorf("drop table: %w", err)
	}
	return executeWithLockRetry(ctx, retry, func(ctx context.Context) error {
		return dropTableAttempt(ctx, pool, schema, table, definition, b)
	}, sleepContext)
}

// dropTableAttempt is one bounded attempt of ExecuteDropTable.
func dropTableAttempt(ctx context.Context, pool *pgxpool.Pool, schema, table, definition string, b Budget) error {
	target := pgx.Identifier{schema, table}.Sanitize()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin drop of %s: %w", target, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way. It runs detached and
		// bounded so a cancelled attempt still releases the table's lock and
		// a wedged socket cannot hold the attempt open.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	// INV: LK-2 — the lock wait, the check, and the drop all run under the
	// caller's transaction-local budgets.
	setBudgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, setBudgets); err != nil {
		return fmt.Errorf("set the budgets of the drop of %s: %w", target, err)
	}
	if err := refuseNonTableBeforeLock(ctx, tx, schema, table, target, b); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE ONLY "+target+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateWrongObjectType {
			// The name now stands for a relation LOCK TABLE refuses, such
			// as a sequence or a materialized view: not the reviewed table.
			return fmt.Errorf("%w: %s is no longer a table; re-plan: %w", ErrDroppedObjectDrift, target, err)
		}
		return dropTableError(err, b, "lock "+target+" to check it before dropping it")
	}
	if err := refuseDroppedTableDrift(ctx, tx, schema, table, definition, target, b); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DROP TABLE "+target); err != nil {
		return dropTableError(err, b, "DROP TABLE "+target)
	}
	if err := tx.Commit(ctx); err != nil {
		// INV: LK-4 — a COMMIT that did not return success may still have
		// landed, so the drop is never reported as rolled back.
		return &CommitOutcomeUnknownError{Err: fmt.Errorf("commit DROP TABLE %s: %w", target, err)}
	}
	return nil
}

// refuseNonTableBeforeLock refuses a name that now stands for a relation
// other than a table, before the attempt locks it. LOCK TABLE on a view
// also locks every relation the view reads, in the same mode, so locking a
// view that replaced the reviewed table would queue ACCESS EXCLUSIVE on the
// tables under it. A name that is missing is left to the lock, which
// reports it. The check under the lock still runs: the name can be
// replaced between this read and the lock.
func refuseNonTableBeforeLock(ctx context.Context, tx pgx.Tx, schema, table, target string, b Budget) error {
	var kind string
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	err := tx.QueryRow(ctx, `
		SELECT c.relkind::pg_catalog.text
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`, schema, table).Scan(&kind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return dropTableError(err, b, "read the kind of "+target+" before locking it")
	case kind == "r" || kind == "p":
		return nil
	}
	return fmt.Errorf("%w: %s is no longer a table (relkind %q), read before locking it; re-plan", ErrDroppedObjectDrift, target, kind)
}

// refuseDroppedTableDrift reads schema.table under the lock the caller
// holds and refuses unless it still has the reviewed definition and is a
// table whose drop removes nothing beyond it and changes no table above it.
func refuseDroppedTableDrift(ctx context.Context, tx pgx.Tx, schema, table, definition, target string, b Budget) error {
	m, err := schemadiff.IntrospectTx(ctx, tx, schema, table)
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		return fmt.Errorf("%w: %s, read under its lock; nothing was dropped", ErrTableNotFound, target)
	case errors.Is(err, schemadiff.ErrNotTable):
		return fmt.Errorf("%w: %s is no longer a table; re-plan: %w", ErrDroppedObjectDrift, target, err)
	case err != nil:
		return dropTableError(err, b, "read "+target+" before dropping it")
	}
	if live := schemadiff.DroppedTableDefinition(m); live != definition {
		return fmt.Errorf("%w: %s is now\n%s\nnot the\n%s\nthe plan reviewed; re-plan",
			ErrDroppedObjectDrift, target, live, definition)
	}
	if m.PartitionKey != "" {
		return fmt.Errorf("%w: %s is partitioned, and dropping a partitioned table drops its partitions, "+
			"which the plan did not review", ErrDroppedObjectHasDescendants, target)
	}
	if len(m.InheritanceChildren) != 0 {
		return fmt.Errorf("%w: %s has inheritance children, and dropping it would drop them too, "+
			"which the plan did not review", ErrDroppedObjectHasDescendants, target)
	}
	if hasParent(m) {
		return fmt.Errorf("%w: %s; dropping it removes rows the table above it holds, which the plan did not review",
			ErrDroppedTableHasParent, target)
	}
	return nil
}

// hasParent reports whether the table's rows are also rows of a table above
// it: it is a partition, or an inheritance child.
func hasParent(m schemadiff.Model) bool {
	return m.IsPartition || len(m.InheritsParents) != 0
}

// dropTableError types a failed statement of a table drop attempt: a
// budget, a missing table or schema, or the server's refusal over objects
// that depend on the table. Anything else names what failed.
func dropTableError(err error, b Budget, what string) error {
	if budgetErr := asBudgetError(err, b); budgetErr != nil {
		return fmt.Errorf("%s: %w", what, budgetErr)
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case sqlstateUndefinedTable, sqlstateInvalidSchemaName:
			return fmt.Errorf("%s: %w; nothing was dropped: %w", what, ErrTableNotFound, err)
		case sqlstateDependentObjectsStillExist:
			return fmt.Errorf("%s: %w; nothing was dropped: %w", what, ErrDroppedTableHasDependents, err)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
