package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// Typed outcomes of a DETACH PARTITION ... CONCURRENTLY step.
var (
	// ErrDetachDefaultPartition is the admission refusal of a concurrent
	// detach from a table that has a DEFAULT partition, which PostgreSQL
	// rejects in every supported version. Nothing is executed.
	ErrDetachDefaultPartition = errors.New("PostgreSQL cannot detach a partition concurrently from a table that has a DEFAULT partition")
	// ErrDetachInterrupted is returned when a concurrent detach stopped
	// after its first transaction committed. The partition is detach-pending:
	// queries through the table no longer read it and rows can no longer be
	// routed into it, but it is still in the partition tree until DETACH
	// PARTITION ... FINALIZE runs. Running the same step again finishes it.
	ErrDetachInterrupted = errors.New("the concurrent detach stopped after its first transaction committed, so the partition is detach-pending")
)

// detachTarget is a concurrent detach's table and partition, resolved to
// the relations they name when the step starts.
type detachTarget struct {
	parentOID    uint32
	partitionOID uint32
	// partition is the partition's name as the error messages quote it.
	partition string
	// finalizeSQL finishes a detach-pending partition; empty when the
	// partition did not resolve, and then nothing can be pending.
	finalizeSQL string
}

// detachPartitionConcurrently runs one admitted DETACH PARTITION ...
// CONCURRENTLY step outside a transaction block, under the brief budgets:
// lock_timeout bounds each lock wait, the wait for older snapshots between
// its two transactions included, and statement_timeout bounds the whole
// statement. A partition left detach-pending by an earlier run is finished
// with DETACH PARTITION ... FINALIZE instead, and finished reports it. A
// lock budget exhausted before the first transaction commits is retried by
// retry like any brief step; once the partition is detach-pending a retry
// runs FINALIZE. A step that stops with the partition still pending returns
// ErrDetachInterrupted.
func detachPartitionConcurrently(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, st statement.Statement, b Budget, retry RetryPolicy) (finished bool, err error) {
	if err := b.validate(); err != nil {
		return false, err
	}
	target, err := resolveDetachTarget(ctx, pool, pt, st, b)
	if err != nil {
		return false, err
	}
	pending, err := target.pending(ctx, pool)
	if err != nil {
		return false, err
	}
	finished = pending
	err = executeWithLockRetry(ctx, retry, func(ctx context.Context) error {
		sql := st.SQL()
		if pending {
			sql = target.finalizeSQL
		}
		runErr := runDetachAttempt(ctx, pool, pt.Schema(), sql, b)
		if runErr == nil {
			return nil
		}
		// The first transaction may have committed before the failure, so
		// the state is read again, even after the caller's context ended.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		left, readErr := target.pending(readCtx, pool)
		if readErr != nil {
			// runErr is kept as text: a budget code would tell the caller
			// to retry, when the partition's state is unknown.
			return fmt.Errorf("detach %s failed (%s), and whether it was left detach-pending is unknown: %w",
				target.partition, runErr.Error(), readErr)
		}
		pending = left
		return runErr
	}, sleepContext)
	if err != nil && pending {
		// err is kept as text, so the outcome code names the pending
		// partition rather than the budget that stopped the wait.
		return false, fmt.Errorf("%w: %s; finish it with %s, or run the same statement again (%s)",
			ErrDetachInterrupted, target.partition, target.finalizeSQL, err.Error())
	}
	return finished, err
}

// runDetachAttempt runs sql on a session of its own, outside a transaction
// block, with b as its session budgets and search_path pinned to schema.
func runDetachAttempt(ctx context.Context, pool *pgxpool.Pool, schema, sql string, b Budget) error {
	conn, release, err := acquireSessionWithBudgets(ctx, pool, b.LockTimeout.Milliseconds(), b.StatementTimeout.Milliseconds(), schema)
	if err != nil {
		return err
	}
	defer release()
	if _, err := conn.Exec(ctx, sql); err != nil {
		if budgetErr := asBudgetError(err, b); budgetErr != nil {
			return budgetErr
		}
		return fmt.Errorf("detach partition: %w", err)
	}
	return nil
}

// resolveDetachTarget resolves the step's table and partition on a session
// whose search_path is the one the step runs under, so an unqualified
// partition resolves to the relation the statement detaches. A partition
// that does not resolve is left for the statement to report.
func resolveDetachTarget(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, st statement.Statement, b Budget) (detachTarget, error) {
	partSchema, partName, ok, err := statement.DetachedPartition(st.SQL())
	if err != nil {
		return detachTarget{}, err
	}
	if !ok {
		return detachTarget{}, fmt.Errorf("%w: %q is not a single DETACH PARTITION", ErrInvariantViolation, st.SQL())
	}
	parent := pgx.Identifier{pt.Schema(), pt.Table()}.Sanitize()
	written := pgx.Identifier{partName}
	if partSchema != "" {
		written = pgx.Identifier{partSchema, partName}
	}
	target := detachTarget{partition: written.Sanitize()}
	conn, release, err := acquireSessionWithBudgets(ctx, pool, b.LockTimeout.Milliseconds(), b.StatementTimeout.Milliseconds(), pt.Schema())
	if err != nil {
		return detachTarget{}, err
	}
	defer release()
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	const q = `
		SELECT pg_catalog.to_regclass($1)::pg_catalog.oid, c.oid, n.nspname, c.relname
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		 WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($2)`
	var schema, name string
	err = conn.QueryRow(ctx, q, parent, target.partition).Scan(&target.parentOID, &target.partitionOID, &schema, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, nil
	}
	if err != nil {
		return detachTarget{}, fmt.Errorf("resolve partition %s of %s: %w", target.partition, parent, err)
	}
	target.partition = pgx.Identifier{schema, name}.Sanitize()
	target.finalizeSQL = "ALTER TABLE " + parent + " DETACH PARTITION " + target.partition + " FINALIZE"
	return target, nil
}

// pending reports whether the partition is detach-pending under the table.
func (t detachTarget) pending(ctx context.Context, q querier) (bool, error) {
	if t.finalizeSQL == "" {
		return false, nil
	}
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	const query = `
		SELECT EXISTS (
		       SELECT 1
		         FROM pg_catalog.pg_inherits h
		        WHERE h.inhparent OPERATOR(pg_catalog.=) $1
		          AND h.inhrelid OPERATOR(pg_catalog.=) $2
		          AND h.inhdetachpending)`
	var pending bool
	if err := q.QueryRow(ctx, query, t.parentOID, t.partitionOID).Scan(&pending); err != nil {
		return false, fmt.Errorf("read whether %s is detach-pending: %w", t.partition, err)
	}
	return pending, nil
}

// AdmitDetachPartition is the catalog half of the admission of a DETACH
// PARTITION ... CONCURRENTLY step: it refuses with
// ErrDetachDefaultPartition, before anything executes, when the table has a
// DEFAULT partition. Any other statement is admitted. The table must be
// schema-qualified. pool must come from pkg/dbconn.
func AdmitDetachPartition(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	st, err := statement.ParseOne(sql)
	if err != nil {
		return err
	}
	concurrent, err := isConcurrentDetach(st)
	if err != nil || !concurrent {
		return err
	}
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	const q = `
		SELECT pg_catalog.quote_ident(dn.nspname) OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident(d.relname)
		  FROM pg_catalog.pg_partitioned_table pt
		  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) pt.partrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  JOIN pg_catalog.pg_class d ON d.oid OPERATOR(pg_catalog.=) pt.partdefid
		  JOIN pg_catalog.pg_namespace dn ON dn.oid OPERATOR(pg_catalog.=) d.relnamespace
		 WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`
	var defaultPartition string
	err = pool.QueryRow(ctx, q, st.Schema(), st.Table()).Scan(&defaultPartition)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the DEFAULT partition of %s: %w", qualifiedName(st.Schema(), st.Table()), err)
	}
	return fmt.Errorf("%w: %s has the DEFAULT partition %s. The only detach PostgreSQL runs on this table is the "+
		"plain DETACH PARTITION, which holds ACCESS EXCLUSIVE on the table and the partition while it runs: submit "+
		"it with the force and destructive acknowledgements for the table to run it as written under the lock and "+
		"statement budgets, or detach the DEFAULT partition first (a plain detach too), after which a concurrent "+
		"detach runs", ErrDetachDefaultPartition, qualifiedName(st.Schema(), st.Table()), defaultPartition)
}

// isConcurrentDetach reports whether st is an ALTER TABLE whose one
// subcommand is DETACH PARTITION ... CONCURRENTLY.
func isConcurrentDetach(st statement.Statement) (bool, error) {
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, err
	}
	return len(ops) == 1 && ops[0].Kind == statement.OpDetachPartition && ops[0].Concurrent, nil
}
