package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrRenamedIndexDrift means the relation an ALTER INDEX ... RENAME TO
// names is no longer a plain index on the preflighted table, read again
// under the table's lock: it is gone, it is not an index (PostgreSQL's
// ALTER INDEX ... RENAME also renames a table), it belongs to another
// table, or it now backs a primary-key, unique, or exclusion constraint,
// which the rename would rename with it. It also means another session
// moved the name to a different relation between that check and the
// rename, so the rename reached a relation other than the one checked.
// Nothing was renamed. The outcome is permanent: re-plan against the
// catalog as it stands.
var ErrRenamedIndexDrift = errors.New("the index the rename names is no longer a plain index on the preflighted table")

// ExecuteIndexRename runs a plain ALTER INDEX ... RENAME TO as bounded
// attempts under transaction-local budgets, retrying only lock_timeout
// failures, bounded by retry. The statement names an index, not a table,
// so the ST-7 binding is checked in the catalog instead of on the
// statement's text. Each attempt pins its search_path to the proof's
// schema alone, so the name the statement carries resolves only there (an
// index's schema is always its table's), then locks the preflighted table
// alone in SHARE UPDATE EXCLUSIVE, which conflicts with every ALTER TABLE
// that could attach a constraint to an index of it, and refuses with
// ErrRenamedIndexDrift unless the name resolves to an index on that table
// that backs no primary-key, unique, or exclusion constraint.
//
// ALTER INDEX ... RENAME locks the index it renames, never its table, so
// the table's lock does not hold the name still: another session can
// rename the checked index away and give its name to another index before
// the statement resolves it. After the statement runs, and before the
// commit, the attempt therefore confirms that the relation now carrying
// the new name in the proof's schema is the index it checked, and rolls
// back with ErrRenamedIndexDrift otherwise. The rename holds that index's
// lock until the commit, so nothing renames it in between.
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
	return executeWithLockRetry(ctx, retry, func(ctx context.Context) error {
		// INV: ST-7 — the rename runs only against the index of the table
		// the preflight proof verified: checked under that table's lock
		// before the statement, and confirmed by identity after it.
		var checked uint32
		before := func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(pt.Schema())); err != nil {
				return fmt.Errorf("pin search_path to %s for the index rename: %w", pt.Schema(), err)
			}
			oid, err := refuseRenamedIndexDrift(ctx, tx, pt, rel)
			checked = oid
			return err
		}
		after := func(ctx context.Context, tx pgx.Tx) error {
			return refuseRenamedOtherRelation(ctx, tx, pt, rel, st.RenamedTo(), checked)
		}
		return executeBoundedAttemptAs(ctx, pool, st, b, pt.Schema(), "", before, after)
	}, sleepContext)
}

// refuseRenamedIndexDrift locks pt's table alone in SHARE UPDATE
// EXCLUSIVE and refuses unless rel names a plain index of that table: an
// index (plain or partitioned) whose owner is the table and which backs no
// primary-key, unique, or exclusion constraint. It returns that index's
// OID.
//
// INV: CO-9 — every function, operator, and relation is
// pg_catalog-qualified, so a planted overload on the search_path cannot
// change which relation the check reads. to_regclass still resolves an
// unqualified index name on the attempt's search_path, exactly as the
// statement will.
func refuseRenamedIndexDrift(ctx context.Context, tx pgx.Tx, pt preflight.PreflightedTable, rel statement.IndexRelation) (uint32, error) {
	table := pgx.Identifier{pt.Schema(), pt.Table()}.Sanitize()
	if _, err := tx.Exec(ctx, "LOCK TABLE ONLY "+table+" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		return 0, fmt.Errorf("lock %s to check the index the rename names: %w", table, err)
	}
	name := pgx.Identifier{rel.Name}
	if rel.Schema != "" {
		name = pgx.Identifier{rel.Schema, rel.Name}
	}
	const q = `
		SELECT i.indexrelid,
		       i.indrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($2),
		       EXISTS (
		           SELECT FROM pg_catalog.pg_constraint con
		           WHERE con.conindid OPERATOR(pg_catalog.=) i.indexrelid
		             AND con.contype OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u', 'x']::pg_catalog."char"[]))
		FROM pg_catalog.pg_index i
		WHERE i.indexrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)`
	var oid uint32
	var onTable, backsConstraint bool
	err := tx.QueryRow(ctx, q, name.Sanitize(), table).Scan(&oid, &onTable, &backsConstraint)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("%w: %s is not an index; nothing was renamed", ErrRenamedIndexDrift, name.Sanitize())
	case err != nil:
		return 0, fmt.Errorf("read index %s on %s: %w", name.Sanitize(), table, err)
	case !onTable:
		return 0, fmt.Errorf("%w: %s is not an index on %s; nothing was renamed", ErrRenamedIndexDrift, name.Sanitize(), table)
	case backsConstraint:
		return 0, fmt.Errorf("%w: %s backs a constraint the rename would rename with it; nothing was renamed",
			ErrRenamedIndexDrift, name.Sanitize())
	}
	return oid, nil
}

// refuseRenamedOtherRelation runs after the rename, in its transaction,
// and refuses unless the relation now named renamedTo in pt's schema is
// checked, the index refuseRenamedIndexDrift read. The search_path holds
// pt's schema alone, so the statement renamed a relation in that schema,
// and a schema's relation names are unique: the relation carrying the new
// name there is the one the statement renamed.
//
// INV: CO-9 — the read is pg_catalog-qualified, as in
// refuseRenamedIndexDrift.
func refuseRenamedOtherRelation(ctx context.Context, tx pgx.Tx, pt preflight.PreflightedTable, rel statement.IndexRelation, renamedTo string, checked uint32) error {
	renamed := pgx.Identifier{pt.Schema(), renamedTo}.Sanitize()
	var oid *uint32
	err := tx.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1)::pg_catalog.oid`, renamed).Scan(&oid)
	if err != nil {
		return fmt.Errorf("confirm %s is the index the rename checked: %w", renamed, err)
	}
	if oid == nil {
		return fmt.Errorf("%w: no relation in %s carries the new name after the rename; nothing was renamed",
			ErrRenamedIndexDrift, pgx.Identifier{pt.Schema()}.Sanitize())
	}
	if *oid != checked {
		return fmt.Errorf("%w: another session gave %s to a different relation before the rename reached it; nothing was renamed",
			ErrRenamedIndexDrift, pgx.Identifier{rel.Name}.Sanitize())
	}
	return nil
}
