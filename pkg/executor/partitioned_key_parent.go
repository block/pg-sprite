package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/block/pg-sprite/pkg/progress"
)

// The parent's step of the partitioned key build: one brief transaction that
// locks the tree, proves every partition carries the constraint its leaf step
// recorded, adds the parent's constraint, and reads back that the constraint
// adopted each partition's constraint and index instead of building one.

// parentKeySQL is the parent's ADD CONSTRAINT, rendered from the admitted
// spec with every identifier quoted, so it carries nothing the spec does not.
func parentKeySQL(target indexTarget, spec partitionedKeySpec) string {
	columns := make([]string, 0, len(spec.columns))
	for _, c := range spec.columns {
		columns = append(columns, pgx.Identifier{c}.Sanitize())
	}
	return "ALTER TABLE " + pgx.Identifier{target.schema, target.table}.Sanitize() +
		" ADD CONSTRAINT " + pgx.Identifier{spec.name}.Sanitize() + " " + spec.kind.clause() +
		" (" + strings.Join(columns, ", ") + ")"
}

// parentKeyOutcome is what the parent's step did. added is false when the
// step found a partition no leaf step had covered and added nothing.
type parentKeyOutcome struct {
	added         bool
	constraintOID uint32
	partitions    int
	serverVersion string
}

// addParentKey runs the parent's step and reports whether it added the
// parent's constraint. A COMMIT whose outcome is unknown is reported as such;
// a rerun finds the constraint and reports it done.
func (f *partitionedKeyFlow) addParentKey(ctx context.Context) (bool, error) {
	alterSQL := parentKeySQL(f.target, f.req.spec)
	f.startStep(progress.OperationBrief, alterSQL)
	var out parentKeyOutcome
	err := f.tx.run(ctx, "add the key constraint of "+qualifiedName(f.target.schema, f.target.table), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = adoptLeafKeys(ctx, tx, f.target, f.req.spec, f.leaves)
			return err
		})
	if err != nil {
		return false, err
	}
	if !out.added {
		return false, nil
	}
	f.rep.ParentAdded, f.rep.ConstraintOID = true, out.constraintOID
	f.rep.Partitions, f.rep.ServerVersion = out.partitions, out.serverVersion
	return true, nil
}

// adoptLeafKeys is the parent's step inside its transaction. The parent's
// ADD CONSTRAINT takes ACCESS EXCLUSIVE on the parent and every partition and
// holds it until the transaction ends. It adopts a partition's unattached
// constraint of the same definition and builds an index only on a partition
// without one, so the step proves, under that same lock, that every partition
// has the constraint its leaf step recorded, and reads back afterwards that
// each was adopted with its index unchanged.
func adoptLeafKeys(ctx context.Context, tx pgx.Tx, target indexTarget, spec partitionedKeySpec, leaves map[uint32]leafKeyResult) (parentKeyOutcome, error) {
	parent := qualifiedName(target.schema, target.table)
	// INV: LK-2 — the tree's ACCESS EXCLUSIVE locks are taken here, each wait
	// bounded by the transaction's lock_timeout, before anything is proved, so
	// the proof below holds until the ADD CONSTRAINT and no attach, detach,
	// or constraint change lands in between.
	if _, err := tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{target.schema, target.table}.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUndefinedTable {
			return parentKeyOutcome{}, fmt.Errorf("%w: ST-7: partitioned table %s no longer exists under that name", ErrInvariantViolation, parent)
		}
		return parentKeyOutcome{}, err
	}
	// INV: ST-7 — the statement names the parent and the server resolves the
	// name, so the name must still be the table the build resolved.
	current, found, err := relationByName(ctx, tx, target.schema, target.table)
	if err != nil {
		return parentKeyOutcome{}, err
	}
	if !found || current.oid != target.tableOID || current.relkind != "p" {
		return parentKeyOutcome{}, fmt.Errorf("%w: ST-7: %s is no longer the partitioned table %d the build resolved",
			ErrInvariantViolation, parent, target.tableOID)
	}
	tree, err := listPartitionTree(ctx, tx, target.tableOID)
	if err != nil {
		return parentKeyOutcome{}, err
	}
	if err := refuseKeyTree(target, tree); err != nil {
		return parentKeyOutcome{}, err
	}
	for _, n := range tree {
		if _, covered := leaves[n.oid]; !covered {
			// A partition appeared since the pass listed the tree. It has no
			// key constraint for the parent to adopt, so the parent's ADD
			// CONSTRAINT would build its index under ACCESS EXCLUSIVE; the
			// step adds nothing and the next pass covers it.
			return parentKeyOutcome{}, nil
		}
	}
	for _, n := range tree {
		if err := proveLeafKeyRecorded(ctx, tx, spec, n, leaves[n.oid]); err != nil {
			return parentKeyOutcome{}, err
		}
	}
	// The parent's columns are proved NOT NULL again under the lock, for the
	// same reason a partition's are (INV: LK-2).
	columns, err := readLeafKeyColumns(ctx, tx, parentNodeOf(target), spec.columns)
	if err != nil {
		return parentKeyOutcome{}, err
	}
	if err := requireParentNotNull(spec, parentNodeOf(target), columns); err != nil {
		return parentKeyOutcome{}, err
	}
	if _, err := tx.Exec(ctx, parentKeySQL(target, spec)); err != nil {
		return parentKeyOutcome{}, err
	}
	constraintOID, err := readBackParentKey(ctx, tx, target, spec, tree, leaves)
	if err != nil {
		return parentKeyOutcome{}, err
	}
	out := parentKeyOutcome{added: true, constraintOID: constraintOID, partitions: len(tree)}
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version')`).Scan(&out.serverVersion); err != nil {
		return parentKeyOutcome{}, fmt.Errorf("read server_version: %w", err)
	}
	return out, nil
}

// proveLeafKeyRecorded proves, under the tree's lock, that the partition has
// exactly one constraint that is the requested key, attached to no parent's
// constraint, and that it is the constraint and index its leaf step recorded.
// Anything else means the partition changed after its leaf step: the parent's
// ADD CONSTRAINT could adopt another constraint or build an index, so the
// step rolls back.
// INV: ST-7 — the parent adopts exactly the constraints the leaf steps made
// or proved.
func proveLeafKeyRecorded(ctx context.Context, q querier, spec partitionedKeySpec, n partitionNode, rec leafKeyResult) error {
	columns, err := readLeafKeyColumns(ctx, q, n, spec.columns)
	if err != nil {
		return err
	}
	constraints, err := readLeafConstraints(ctx, q, n)
	if err != nil {
		return err
	}
	match, err := matchingLeafConstraint(spec, n, leafKeyFacts{columns: columns, constraints: constraints}, leafKeyDefinitionKey(columns))
	if err != nil {
		return err
	}
	if match == nil {
		return fmt.Errorf("%w: ST-7: partition %s no longer has the key constraint %s (%d) its leaf step left",
			ErrInvariantViolation, n.qualified(), rec.constraint, rec.constraintOID)
	}
	if match.oid != rec.constraintOID || match.index.oid != rec.indexOID {
		return fmt.Errorf("%w: ST-7: the key constraint of partition %s is %s (%d) on index %d, not the constraint %s (%d) on index %d its leaf step left",
			ErrInvariantViolation, n.qualified(), match.name, match.oid, match.index.oid, rec.constraint, rec.constraintOID, rec.indexOID)
	}
	return requireNotNullForPrimaryKey(spec, n, columns)
}

const parentKeyReadBackSQL = `SELECT k.oid, k.contype::pg_catalog.text, k.conindid
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		    AND k.conname OPERATOR(pg_catalog.=) $2`

const adoptedLeafKeySQL = `SELECT k.oid, k.conindid
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		    AND k.conparentid OPERATOR(pg_catalog.=) $2`

// readBackParentKey reads, in the transaction that ran the parent's ADD
// CONSTRAINT, what it did, and returns the parent constraint's OID: a
// constraint of the requested kind under the requested name, backed by a
// valid partitioned index, that adopted on every partition exactly the
// constraint the leaf step recorded, whose index is now attached to the
// parent's index under its recorded OID. Any other outcome rolls the step
// back.
// INV: LK-2 — every partition's index OID unchanged is the proof the ADD
// CONSTRAINT built nothing under its ACCESS EXCLUSIVE lock and only changed
// the catalog.
func readBackParentKey(ctx context.Context, q querier, target indexTarget, spec partitionedKeySpec, tree []partitionNode, leaves map[uint32]leafKeyResult) (uint32, error) {
	parent := qualifiedName(target.schema, target.table)
	var (
		constraintOID uint32
		contype       string
		indexOID      uint32
	)
	if err := q.QueryRow(ctx, parentKeyReadBackSQL, target.tableOID, spec.name).Scan(&constraintOID, &contype, &indexOID); err != nil {
		return 0, fmt.Errorf("read back the key constraint %s of %s: %w", spec.name, parent, err)
	}
	if contype != spec.kind.contype() {
		return 0, fmt.Errorf("%w: after ADD CONSTRAINT, constraint %s on %s is not a %s",
			ErrInvariantViolation, spec.name, parent, spec.kind.clause())
	}
	idx, found, err := relationByOID(ctx, q, indexOID)
	if err != nil {
		return 0, err
	}
	if !found || !idx.isPartitionedIndex() || idx.tableOID != target.tableOID || !idx.valid {
		return 0, fmt.Errorf("%w: after ADD CONSTRAINT, the index %d of constraint %s is not a valid partitioned index on %s",
			ErrInvariantViolation, indexOID, spec.name, parent)
	}
	for _, n := range tree {
		rec := leaves[n.oid]
		var adoptedOID, adoptedIndex uint32
		err := q.QueryRow(ctx, adoptedLeafKeySQL, n.oid, constraintOID).Scan(&adoptedOID, &adoptedIndex)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("%w: after ADD CONSTRAINT, partition %s has no constraint adopted by %s",
				ErrInvariantViolation, n.qualified(), spec.name)
		}
		if err != nil {
			return 0, fmt.Errorf("read the adopted key constraint of partition %s: %w", n.qualified(), err)
		}
		if adoptedOID != rec.constraintOID || adoptedIndex != rec.indexOID {
			return 0, fmt.Errorf("%w: LK-2: after ADD CONSTRAINT, partition %s's adopted constraint is %d on index %d, not the constraint %d on index %d its leaf step left; the statement built or chose another",
				ErrInvariantViolation, n.qualified(), adoptedOID, adoptedIndex, rec.constraintOID, rec.indexOID)
		}
		attached, ok, err := attachedIndexOf(ctx, q, n.oid, indexOID)
		if err != nil {
			return 0, err
		}
		if !ok || attached != rec.indexOID {
			return 0, fmt.Errorf("%w: LK-2: after ADD CONSTRAINT, the index of partition %s attached to %s is not index %d its leaf step left",
				ErrInvariantViolation, n.qualified(), spec.name, rec.indexOID)
		}
	}
	return constraintOID, nil
}
