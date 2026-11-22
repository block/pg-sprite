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

// read runs fn against the catalog in a read-only transaction under
// catalogReadPath.
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

// onlyIndexTable is the identity a metadata-only index is created against: a
// partitioned table, by OID, with the schema and name the statement resolves.
type onlyIndexTable struct {
	oid    uint32
	schema string
	name   string
}

func (t onlyIndexTable) qualified() string { return qualifiedName(t.schema, t.name) }

// lockAndProveTable takes a bounded lock on the table the statement will name
// and proves, under it, that the name still answers to the table's identity. A
// statement names a table and the server resolves the name when it runs, so a
// table renamed away and replaced under the same name would otherwise receive
// the index. The lock is the weakest one that excludes a rename, taken under the
// transaction's own lock_timeout (LK-2), and held until the transaction ends.
// INV: ST-7 — the statement runs against the table that was proven.
func lockAndProveTable(ctx context.Context, tx pgx.Tx, table onlyIndexTable) error {
	if err := lockRelationShared(ctx, tx, table.schema, table.name, table.qualified()); err != nil {
		return err
	}
	var proven bool
	if err := tx.QueryRow(ctx, provePartitionedTableSQL, table.oid, table.schema, table.name).Scan(&proven); err != nil {
		return fmt.Errorf("prove table %s: %w", table.qualified(), err)
	}
	if !proven {
		return fmt.Errorf("%w: ST-7: %s is not the partitioned table (%d) the build resolved: it was renamed or replaced since",
			ErrInvariantViolation, table.qualified(), table.oid)
	}
	return nil
}

// lockRelationShared locks a relation by name in SHARE UPDATE EXCLUSIVE mode,
// the weakest mode that excludes a rename and a detach, and which blocks no
// writer. A name that no longer exists is an ST-7 refusal.
func lockRelationShared(ctx context.Context, tx pgx.Tx, schema, name, display string) error {
	_, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{schema, name}.Sanitize()+" IN SHARE UPDATE EXCLUSIVE MODE")
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUndefinedTable {
		return fmt.Errorf("%w: ST-7: %s no longer exists under that name; it was renamed or dropped since the build resolved it",
			ErrInvariantViolation, display)
	}
	return err
}

const provePartitionedTableSQL = `SELECT EXISTS (
		   SELECT 1
		     FROM pg_catalog.pg_class c
		     JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		    WHERE c.oid OPERATOR(pg_catalog.=) $1
		      AND n.nspname OPERATOR(pg_catalog.=) $2
		      AND c.relname OPERATOR(pg_catalog.=) $3
		      AND c.relkind OPERATOR(pg_catalog.=) 'p')`

// createOnlyIndex runs a CREATE INDEX ... ON ONLY statement in tx on a table it
// has locked and proved, and returns the index it created, read under the
// catalog-only search_path. The index is checked by identity before the
// transaction can commit: it must be a partitioned index on the proved table
// (INV: ST-7), and any other outcome fails the step, which rolls back.
func createOnlyIndex(ctx context.Context, tx pgx.Tx, table onlyIndexTable, indexSchema, indexName, createSQL string) (relationFacts, error) {
	if err := lockAndProveTable(ctx, tx, table); err != nil {
		return relationFacts{}, err
	}
	if _, err := tx.Exec(ctx, createSQL); err != nil {
		return relationFacts{}, err
	}
	// The definition is printed under the catalog-only path, the one every
	// later read of the index uses.
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(catalogReadPath()...)); err != nil {
		return relationFacts{}, fmt.Errorf("scope the search path to read the new index: %w", err)
	}
	created, found, err := relationByName(ctx, tx, indexSchema, indexName)
	if err != nil {
		return relationFacts{}, err
	}
	if !found {
		return relationFacts{}, fmt.Errorf("%w: index %s is not in the catalog after it was created",
			ErrInvariantViolation, qualifiedName(indexSchema, indexName))
	}
	if !created.isPartitionedIndex() || created.tableOID != table.oid {
		return relationFacts{}, fmt.Errorf("%w: ST-7: index %s was created on %s, not on %s",
			ErrInvariantViolation, qualifiedName(indexSchema, indexName), describeRelation(created), table.qualified())
	}
	return created, nil
}
