package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/block/pg-sprite/pkg/progress"
)

// The parent's step of the partitioned foreign key build: one brief
// transaction that locks the tree and the referenced table, proves every
// partition carries the validated foreign key its leaf step recorded, adds
// the parent's foreign key, and reads back that it adopted each partition's
// instead of creating and validating one.

// parentFKSQL is the parent's ADD FOREIGN KEY, rendered from the admitted
// spec with every identifier quoted, so it carries nothing the spec does not.
func parentFKSQL(target indexTarget, spec partitionedFKSpec) string {
	return "ALTER TABLE " + pgx.Identifier{target.schema, target.table}.Sanitize() +
		" ADD CONSTRAINT " + pgx.Identifier{spec.name}.Sanitize() + " " + spec.clause()
}

// parentFKOutcome is what the parent's step did. added is false when the
// step found a partition no leaf step had covered and added nothing.
type parentFKOutcome struct {
	added         bool
	constraintOID uint32
	partitions    int
	serverVersion string
}

// addParentFK runs the parent's step and reports whether it added the
// parent's foreign key. A COMMIT whose outcome is unknown is reported as
// such; a rerun finds the foreign key and reports it done.
func (f *partitionedFKFlow) addParentFK(ctx context.Context) (bool, error) {
	f.startStep(progress.OperationBrief, parentFKSQL(f.target, f.req.spec))
	var out parentFKOutcome
	err := f.tx.run(ctx, "add the foreign key of "+qualifiedName(f.target.schema, f.target.table), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = adoptLeafFKs(ctx, tx, f.target, f.fk, f.leaves)
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

// adoptLeafFKs is the parent's step inside its transaction. The parent's ADD
// FOREIGN KEY takes SHARE ROW EXCLUSIVE on the parent, every partition, and
// the referenced table, and ACCESS EXCLUSIVE on the referenced table while
// it drops each adopted foreign key's own triggers there; it holds them
// until the transaction ends. It adopts a partition's validated, unattached
// foreign key of the same definition, and creates and validates one, a scan
// under those locks, on a partition without one. So the step proves, under
// the same locks, that every partition has the foreign key its leaf step
// recorded, and reads back afterwards that each was adopted under its
// recorded OID.
func adoptLeafFKs(ctx context.Context, tx pgx.Tx, target indexTarget, fk resolvedFK, leaves map[uint32]leafFKResult) (parentFKOutcome, error) {
	parent := qualifiedName(target.schema, target.table)
	// INV: LK-2 — the tree's SHARE ROW EXCLUSIVE locks, the ones the ADD
	// FOREIGN KEY takes, are taken here, each wait bounded by the
	// transaction's lock_timeout, before anything is proved. They exclude
	// every attach, detach, and constraint change on the tree until the
	// transaction ends, so the proof below holds through the statement.
	if _, err := tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{target.schema, target.table}.Sanitize()+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUndefinedTable {
			return parentFKOutcome{}, fmt.Errorf("%w: ST-7: partitioned table %s no longer exists under that name", ErrInvariantViolation, parent)
		}
		return parentFKOutcome{}, err
	}
	// INV: ST-7 — the statement names the parent and the referenced table,
	// and the server resolves the names, so each must still be the table the
	// build resolved.
	current, found, err := relationByName(ctx, tx, target.schema, target.table)
	if err != nil {
		return parentFKOutcome{}, err
	}
	if !found || current.oid != target.tableOID || current.relkind != "p" {
		return parentFKOutcome{}, fmt.Errorf("%w: ST-7: %s is no longer the partitioned table %d the build resolved",
			ErrInvariantViolation, parent, target.tableOID)
	}
	if err := lockAndProveReference(ctx, tx, fk.ref); err != nil {
		return parentFKOutcome{}, err
	}
	tree, err := listPartitionTree(ctx, tx, target.tableOID)
	if err != nil {
		return parentFKOutcome{}, err
	}
	if err := refuseFKTree(target, fk.ref, tree); err != nil {
		return parentFKOutcome{}, err
	}
	for _, n := range tree {
		if _, covered := leaves[n.oid]; !covered {
			// A partition appeared since the pass listed the tree. It has no
			// foreign key for the parent to adopt, so the parent's statement
			// would validate it under the tree's locks; the step adds
			// nothing and the next pass covers it.
			return parentFKOutcome{}, nil
		}
	}
	for _, n := range tree {
		if err := proveLeafFKRecorded(ctx, tx, fk, n, leaves[n.oid]); err != nil {
			return parentFKOutcome{}, err
		}
	}
	if _, err := tx.Exec(ctx, parentFKSQL(target, fk.spec)); err != nil {
		return parentFKOutcome{}, err
	}
	constraintOID, err := readBackParentFK(ctx, tx, target, fk, tree, leaves)
	if err != nil {
		return parentFKOutcome{}, err
	}
	out := parentFKOutcome{added: true, constraintOID: constraintOID, partitions: len(tree)}
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version')`).Scan(&out.serverVersion); err != nil {
		return parentFKOutcome{}, fmt.Errorf("read server_version: %w", err)
	}
	return out, nil
}

// lockAndProveReference locks the referenced table in SHARE ROW EXCLUSIVE,
// after the tree's locks, the order the parent's ADD FOREIGN KEY takes them,
// and proves under it that its name still resolves to the table the build
// resolved. A rename needs ACCESS EXCLUSIVE, which the lock excludes until
// the transaction ends, so the statement that names the table binds the one
// proved here (INV: ST-7).
func lockAndProveReference(ctx context.Context, tx pgx.Tx, ref fkReference) error {
	_, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{ref.schema, ref.table}.Sanitize()+" IN SHARE ROW EXCLUSIVE MODE")
	if err != nil {
		return err
	}
	current, found, err := relationByName(ctx, tx, ref.schema, ref.table)
	if err != nil {
		return err
	}
	if !found || current.oid != ref.oid {
		return fmt.Errorf("%w: ST-7: %s no longer names the referenced table %d the build resolved",
			ErrInvariantViolation, qualifiedName(ref.schema, ref.table), ref.oid)
	}
	return nil
}

// proveLeafFKRecorded proves, under the tree's lock, that the partition has
// exactly one foreign key the parent's statement could adopt (isAdoptable:
// validated, attached to no parent's, and of the requested definition apart
// from any ON DELETE column list), and that it is the one its leaf step
// recorded. Anything else means the partition changed after its leaf step:
// the parent's statement could adopt another foreign key or validate a new
// one under the tree's locks, so the step rolls back with
// ErrPartitionedForeignKeyLeafChanged, and a rerun decides the partition
// again; a rerun that finds a column-list foreign key among them refuses it
// with ErrPartitionedForeignKeyConflict.
// INV: ST-7 — the parent adopts exactly the foreign keys the leaf steps made
// or proved.
func proveLeafFKRecorded(ctx context.Context, q querier, fk resolvedFK, n partitionNode, rec leafFKResult) error {
	attnums, err := fkColumnAttnums(ctx, q, n.oid, n.qualified(), fk.spec.columns)
	if err != nil {
		return err
	}
	fks, err := readForeignKeys(ctx, q, n.oid, n.qualified(), fk.serverMajor)
	if err != nil {
		return err
	}
	// Adoptable, not requested: the parent's statement could adopt any of
	// them, a column-list foreign key included, so the leaf step's must be
	// the only one.
	matches := adoptableForeignKeys(fks, fk.spec, attnums, fk.ref)
	if len(matches) != 1 {
		return fmt.Errorf("%w: partition %s has %d foreign keys the parent's could adopt, not the one %s (%d) its leaf step left",
			ErrPartitionedForeignKeyLeafChanged, n.qualified(), len(matches), rec.constraint, rec.constraintOID)
	}
	if !matches[0].isRecorded(rec) {
		return fmt.Errorf("%w: the foreign key of partition %s is %s (%d, validated %t, parent %d), not the validated, unattached %s (%d) its leaf step left",
			ErrPartitionedForeignKeyLeafChanged, n.qualified(), matches[0].name, matches[0].oid, matches[0].validated, matches[0].parentOID,
			rec.constraint, rec.constraintOID)
	}
	return nil
}

// isRecorded reports whether the foreign key is the one a leaf step left,
// validated and adopted by nothing.
func (k catalogFK) isRecorded(rec leafFKResult) bool {
	return k.oid == rec.constraintOID && k.validated && k.parentOID == 0
}

const adoptedFKsSQL = `SELECT k.oid, k.convalidated
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		    AND k.conparentid OPERATOR(pg_catalog.=) $2
		  ORDER BY k.oid`

// adoptedFK is a partition's constraint a parent's adopted.
type adoptedFK struct {
	oid       uint32
	validated bool
}

// adoptedFKs lists the partition's constraints the parent's constraint
// parentOID adopted.
func adoptedFKs(ctx context.Context, q querier, n partitionNode, parentOID uint32) ([]adoptedFK, error) {
	rows, err := q.Query(ctx, adoptedFKsSQL, n.oid, parentOID)
	if err != nil {
		return nil, fmt.Errorf("list the adopted foreign keys of partition %s: %w", n.qualified(), err)
	}
	defer rows.Close()
	var out []adoptedFK
	for rows.Next() {
		var a adoptedFK
		if err := rows.Scan(&a.oid, &a.validated); err != nil {
			return nil, fmt.Errorf("scan the adopted foreign keys of partition %s: %w", n.qualified(), err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the adopted foreign keys of partition %s: %w", n.qualified(), err)
	}
	return out, nil
}

// readBackParentFK reads, in the transaction that ran the parent's ADD
// FOREIGN KEY, what it did, and returns the parent foreign key's OID: a
// validated foreign key of the requested definition under the requested
// name that adopted, on every partition, exactly one foreign key, the one
// the leaf step recorded, under its recorded OID. A partition the statement
// gave a new foreign key instead, which it created and validated under the
// tree's locks, shows a new OID there. Any other outcome rolls the step back.
// INV: LK-2 — every partition's foreign key OID unchanged is the proof the
// ADD FOREIGN KEY validated nothing under its locks and only changed the
// catalog.
func readBackParentFK(ctx context.Context, q querier, target indexTarget, fk resolvedFK, tree []partitionNode, leaves map[uint32]leafFKResult) (uint32, error) {
	parent := qualifiedName(target.schema, target.table)
	attnums, err := fkColumnAttnums(ctx, q, target.tableOID, parent, fk.spec.columns)
	if err != nil {
		return 0, err
	}
	fks, err := readForeignKeys(ctx, q, target.tableOID, parent, fk.serverMajor)
	if err != nil {
		return 0, err
	}
	var added *catalogFK
	for i := range fks {
		if fks[i].name == fk.spec.name {
			added = &fks[i]
		}
	}
	if added == nil {
		return 0, fmt.Errorf("%w: after ADD FOREIGN KEY, %s has no foreign key %s", ErrInvariantViolation, parent, fk.spec.name)
	}
	if !added.isRequested(fk.spec, attnums, fk.ref) {
		return 0, fmt.Errorf("%w: after ADD FOREIGN KEY, foreign key %s on %s does not have the requested definition",
			ErrInvariantViolation, fk.spec.name, parent)
	}
	if !added.validated {
		return 0, fmt.Errorf("%w: after ADD FOREIGN KEY, foreign key %s on %s is not validated", ErrInvariantViolation, fk.spec.name, parent)
	}
	for _, n := range tree {
		rec := leaves[n.oid]
		adopted, err := adoptedFKs(ctx, q, n, added.oid)
		if err != nil {
			return 0, err
		}
		if !adoptedOnlyRecorded(adopted, rec) {
			return 0, fmt.Errorf("%w: LK-2: after ADD FOREIGN KEY, partition %s's foreign keys adopted by %s are %v, not the validated foreign key %d its leaf step left; the statement created another",
				ErrInvariantViolation, n.qualified(), fk.spec.name, adopted, rec.constraintOID)
		}
	}
	return added.oid, nil
}

// adoptedOnlyRecorded reports whether a partition's adopted foreign keys are
// exactly the one its leaf step left, validated.
func adoptedOnlyRecorded(adopted []adoptedFK, rec leafFKResult) bool {
	if len(adopted) != 1 {
		return false
	}
	return adopted[0].oid == rec.constraintOID && adopted[0].validated
}
