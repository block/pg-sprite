package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrAdoptedIndexDrift means the index a planned ADD CONSTRAINT ... USING
// INDEX takes over no longer has the definition the plan matched it
// against, read again under the table's lock: it was dropped, replaced
// under the same name, or left invalid after planning. Nothing was
// attached. The outcome is permanent: re-plan against the index as it
// stands.
var ErrAdoptedIndexDrift = errors.New("the index the constraint takes over changed after planning")

// adoptsIndex reports whether st is an ADD CONSTRAINT ... USING INDEX that
// takes over the index named index.
func adoptsIndex(st statement.Statement, index string) (bool, error) {
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	return slices.ContainsFunc(ops, func(op statement.Op) bool {
		return op.Kind == statement.OpAddConstraint && op.UsingIndex && slices.Contains(op.Indexes, index)
	}), nil
}

// refuseAdoptedIndexDrift locks st's table in ACCESS EXCLUSIVE, the lock
// the ADD CONSTRAINT takes anyway and one that blocks every DROP INDEX,
// CREATE INDEX, and rename on the table, then reads the index the
// constraint takes over and refuses unless it is still valid and still has
// the definition a. Holding the lock until the constraint commits means
// the index cannot change between this read and the attachment. The
// definition is unqualified the way the diff's introspection unqualifies
// it, so the two compare as the diff compared them.
func refuseAdoptedIndexDrift(ctx context.Context, tx pgx.Tx, st statement.Statement, a schemadiff.IndexAdoption) error {
	target, err := lockForCheck(ctx, tx, st, "check the index its constraint takes over")
	if err != nil {
		return err
	}
	var def string
	var valid bool
	err = tx.QueryRow(ctx, `SELECT pg_catalog.pg_get_indexdef(i.indexrelid), i.indisvalid
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = $1::regclass AND c.relname = $2`, target, a.Index).Scan(&def, &valid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s has no index named %s any more; re-plan", ErrAdoptedIndexDrift, target, a.Index)
	case err != nil:
		return fmt.Errorf("read index %s on %s: %w", a.Index, target, err)
	}
	if def, err = statement.Qualify(def, ""); err != nil {
		return fmt.Errorf("unqualify index %s on %s: %w", a.Index, target, err)
	}
	switch {
	case !valid:
		return fmt.Errorf("%w: index %s on %s is invalid; re-plan", ErrAdoptedIndexDrift, a.Index, target)
	case def != a.Def:
		return fmt.Errorf("%w: index %s on %s is now %q, not the %q the plan matched; re-plan",
			ErrAdoptedIndexDrift, a.Index, target, def, a.Def)
	}
	return nil
}

// lockForCheck takes ACCESS EXCLUSIVE on st's table alone (ONLY), which the
// statement locks anyway, inside tx so a catalog read made next stays true
// until tx commits, and returns the table's quoted name. A caller that also
// reads the tables below locks them itself, and only when its statement
// reaches them. A lock not granted within the attempt's lock_timeout fails
// like the statement itself would.
func lockForCheck(ctx context.Context, tx pgx.Tx, st statement.Statement, why string) (string, error) {
	return lockTableForCheck(ctx, tx, st, "ACCESS EXCLUSIVE", why)
}

// lockTableForCheck is lockForCheck in the given lock mode, for a
// statement that takes a weaker lock than ACCESS EXCLUSIVE and must not
// block more than it does.
func lockTableForCheck(ctx context.Context, tx pgx.Tx, st statement.Statement, mode, why string) (string, error) {
	target := pgx.Identifier{st.Table()}
	if st.Schema() != "" {
		target = pgx.Identifier{st.Schema(), st.Table()}
	}
	name := target.Sanitize()
	if _, err := tx.Exec(ctx, "LOCK TABLE ONLY "+name+" IN "+mode+" MODE"); err != nil {
		return "", fmt.Errorf("lock %s to %s: %w", name, why, err)
	}
	return name, nil
}
