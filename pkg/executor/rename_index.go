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

// ErrRenamedIndexDrift means the relation an ALTER INDEX ... RENAME TO
// names is no longer a plain index on the preflighted table, read again
// under the table's lock: it is gone, it is not an index (PostgreSQL's
// ALTER INDEX ... RENAME also renames a table), it belongs to another
// table, or it now backs a primary-key, unique, or exclusion constraint,
// which the rename would rename with it. Nothing was renamed. The outcome
// is permanent: re-plan against the catalog as it stands.
var ErrRenamedIndexDrift = errors.New("the index the rename names is no longer a plain index on the preflighted table")

// ExecuteIndexRename runs a plain ALTER INDEX ... RENAME TO as bounded
// attempts under transaction-local budgets, retrying only lock_timeout
// failures, bounded by retry. The statement names an index, not a table,
// so the ST-7 binding is checked in the catalog instead of on the
// statement's text: each attempt locks the preflighted table alone in
// SHARE UPDATE EXCLUSIVE — the lock the rename takes on the index, which
// conflicts with every ALTER TABLE that could attach a constraint to it —
// then refuses with ErrRenamedIndexDrift unless the name the statement
// carries resolves, under the attempt's own search_path, to an index on
// that table that backs no primary-key, unique, or exclusion constraint.
// Holding the lock until the rename commits keeps the check true. An
// index's schema is always its table's, so the search_path pinned to the
// proof's schema resolves an unqualified index name there first.
func ExecuteIndexRename(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, st statement.Statement, b Budget, retry RetryPolicy) error {
	if err := b.validate(); err != nil {
		return err
	}
	if err := retry.validate(); err != nil {
		return err
	}
	rel := st.IndexRelation()
	if st.Kind() != statement.KindRenameIndex || rel.Kind != statement.IndexRelationIndex {
		return fmt.Errorf("%w: ExecuteIndexRename runs ALTER INDEX ... RENAME TO, got %s", ErrInvariantViolation, st.Kind())
	}
	// INV: ST-7 — the rename runs only against the index of the table the
	// preflight proof verified, checked under that table's lock.
	before := func(ctx context.Context, tx pgx.Tx) error {
		return refuseRenamedIndexDrift(ctx, tx, pt, rel)
	}
	return executeWithLockRetry(ctx, retry, func(ctx context.Context) error {
		return executeBoundedAttemptAs(ctx, pool, st, b, pt.Schema(), "", before)
	}, sleepContext)
}

// refuseRenamedIndexDrift locks pt's table alone in SHARE UPDATE
// EXCLUSIVE and refuses unless rel names a plain index of that table: an
// index (plain or partitioned) whose owner is the table and which backs no
// primary-key, unique, or exclusion constraint.
//
// INV: CO-9 — every function, operator, and relation is
// pg_catalog-qualified, so a planted overload on the search_path cannot
// change which relation the check reads. to_regclass still resolves an
// unqualified index name on the attempt's search_path, exactly as the
// statement will.
func refuseRenamedIndexDrift(ctx context.Context, tx pgx.Tx, pt preflight.PreflightedTable, rel statement.IndexRelation) error {
	table := pgx.Identifier{pt.Schema(), pt.Table()}.Sanitize()
	if _, err := tx.Exec(ctx, "LOCK TABLE ONLY "+table+" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock %s to check the index the rename names: %w", table, err)
	}
	name := pgx.Identifier{rel.Name}
	if rel.Schema != "" {
		name = pgx.Identifier{rel.Schema, rel.Name}
	}
	const q = `
		SELECT i.indrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($2),
		       EXISTS (
		           SELECT FROM pg_catalog.pg_constraint con
		           WHERE con.conindid OPERATOR(pg_catalog.=) i.indexrelid
		             AND con.contype OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u', 'x']::pg_catalog."char"[]))
		FROM pg_catalog.pg_index i
		WHERE i.indexrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)`
	var onTable, backsConstraint bool
	err := tx.QueryRow(ctx, q, name.Sanitize(), table).Scan(&onTable, &backsConstraint)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s is not an index; nothing was renamed", ErrRenamedIndexDrift, name.Sanitize())
	case err != nil:
		return fmt.Errorf("read index %s on %s: %w", name.Sanitize(), table, err)
	case !onTable:
		return fmt.Errorf("%w: %s is not an index on %s; nothing was renamed", ErrRenamedIndexDrift, name.Sanitize(), table)
	case backsConstraint:
		return fmt.Errorf("%w: %s backs a constraint the rename would rename with it; nothing was renamed",
			ErrRenamedIndexDrift, name.Sanitize())
	}
	return nil
}
