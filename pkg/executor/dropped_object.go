package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrDroppedObjectDrift means the column, constraint, or NOT NULL a
// reviewed plan drops no longer has the definition the plan carried for it,
// read again under the table's lock: it is gone, or the name now stands
// for another object (a column dropped and added again, a constraint
// replaced under its name). Nothing was dropped. The outcome is permanent:
// re-plan against the table as it stands and review the new plan.
var ErrDroppedObjectDrift = errors.New("the object the plan drops changed after planning")

// ErrDroppedObjectHasDescendants means a reviewed column, constraint, or
// NOT NULL drop names a table that has partitions or inheritance children,
// read under the table's lock. The drop would reach into those tables too:
// a column drop removes their copies of the column, a constraint drop
// removes their copies of the constraint (and, for a partitioned table's
// key, each partition's index), and a NOT NULL drop clears it on their
// copies. The plan reviewed only the named table's object and its
// dependents, so nothing was dropped. The outcome is permanent.
var ErrDroppedObjectHasDescendants = errors.New("the object the plan drops also reaches tables below it")

// opDrops reports whether op drops the column, constraint, or NOT NULL
// that d names. An index drop is a DROP INDEX statement and never matches
// here (see ExecuteDropIndexConcurrently).
func opDrops(op statement.Op, d *schemadiff.DroppedObject) bool {
	switch d.Kind {
	case schemadiff.DroppedColumn:
		return op.Kind == statement.OpDropColumn && op.Column == d.Name
	case schemadiff.DroppedConstraint:
		return op.Kind == statement.OpDropConstraint && op.Name == d.Name
	case schemadiff.DroppedNotNull:
		return op.Kind == statement.OpDropNotNull && op.Column == d.Name
	default:
		return false
	}
}

// dropsObject reports whether st is an ALTER TABLE whose only operation
// drops the column, constraint, or NOT NULL that d names. It is an error
// for st to drop d in any other shape, because only a step whose sole
// operation is the drop is checked against the plan: a statement that
// drops d alongside other operations is refused. Any operation that says
// CASCADE is refused too, whichever object it names and however many
// operations the statement has: it would remove objects no plan lists, and
// a plan never drops with CASCADE.
func dropsObject(st statement.Statement, d *schemadiff.DroppedObject) (bool, error) {
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	for _, op := range ops {
		if op.Cascade {
			return false, fmt.Errorf("%w: %q drops with CASCADE, which removes objects the plan did not review", ErrInvariantViolation, st.SQL())
		}
	}
	for _, op := range ops {
		if !opDrops(op, d) {
			continue
		}
		if len(ops) != 1 {
			return false, fmt.Errorf("%w: %q drops %s %s alongside other operations, and only a step whose sole operation is the drop is checked",
				ErrInvariantViolation, st.SQL(), d.Kind, d.Name)
		}
		return true, nil
	}
	return false, nil
}

// refuseDroppedObjectDrift locks st's table in ACCESS EXCLUSIVE, the lock
// every drop it checks takes anyway, then reads the object the statement
// drops and refuses unless it still has the definition d carries and
// dropping it takes along nothing that d does not list. Holding
// the lock until the drop commits means the object cannot change between
// this read and the drop. The read runs under the attempt's search_path,
// the one the plan's definitions were read under, so the two print alike.
func refuseDroppedObjectDrift(ctx context.Context, tx pgx.Tx, st statement.Statement, d schemadiff.DroppedObject) error {
	if st.Schema() == "" {
		return fmt.Errorf("%w: %q drops %s %s of a table with no schema", ErrInvariantViolation, st.SQL(), d.Kind, d.Name)
	}
	target, err := lockForCheck(ctx, tx, st, "check the "+string(d.Kind)+" it drops")
	if err != nil {
		return err
	}
	live, found, err := schemadiff.ReadDroppedObject(ctx, tx, st.Schema(), st.Table(), d.Kind, d.Name)
	if err != nil {
		return fmt.Errorf("read %s %s of %s before dropping it: %w", d.Kind, d.Name, target, err)
	}
	if !found {
		return fmt.Errorf("%w: %s has no %s named %s any more; re-plan", ErrDroppedObjectDrift, target, d.Kind, d.Name)
	}
	if live.Definition != d.Definition {
		return fmt.Errorf("%w: %s %s of %s is now %q, not the %q the plan reviewed; re-plan",
			ErrDroppedObjectDrift, d.Kind, d.Name, target, live.Definition, d.Definition)
	}
	if extra := schemadiff.UnlistedDependents(d.Dependents, live.Dependents); len(extra) > 0 {
		return fmt.Errorf("%w: dropping %s %s of %s would also remove %s, which the plan did not review; re-plan",
			ErrDroppedObjectDrift, d.Kind, d.Name, target, strings.Join(extra, "; "))
	}
	return refuseDropBelow(ctx, tx, st, d, target)
}

// refuseDropBelow refuses a drop on a table that has partitions or
// inheritance children, read under the table's lock. A drop without ONLY
// reaches their copies of the object too, and with them anything that
// depends on a copy, none of which the plan reviewed. A drop with ONLY is
// refused as well: it would leave the children's copies standing, which is
// not the change the plan reviewed either, and a plan never says ONLY.
func refuseDropBelow(ctx context.Context, tx pgx.Tx, st statement.Statement, d schemadiff.DroppedObject, target string) error {
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	var below bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
		  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.inhparent
		  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		 WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2)`,
		st.Schema(), st.Table()).Scan(&below); err != nil {
		return fmt.Errorf("read the tables below %s before dropping %s %s: %w", target, d.Kind, d.Name, err)
	}
	if below {
		return fmt.Errorf("%w: %s has partitions or inheritance children, and dropping %s %s would reach their copies "+
			"and anything on them, which the plan did not review", ErrDroppedObjectHasDescendants, target, d.Kind, d.Name)
	}
	return nil
}

// droppingStep returns the index of the admitted step that drops d, or -1
// when d is nil. A planned drop no step performs, or one a step other than
// a brief native attempt performs, is a breach of the plan's own contract
// and refuses before anything runs.
func droppingStep(admitted []sequenceStep, d *schemadiff.DroppedObject) (int, error) {
	if d == nil {
		return -1, nil
	}
	for i, step := range admitted {
		ok, err := dropsObject(step.st, d)
		if err != nil {
			return -1, err
		}
		if !ok {
			continue
		}
		if step.kind != StepBrief {
			return -1, fmt.Errorf("%w: step %d drops %s %s as a %s step, not a brief one",
				ErrInvariantViolation, i+1, d.Kind, d.Name, step.kind)
		}
		// Only this step is checked, so a later step that drops the same
		// name would drop whatever took it in between, unchecked. An
		// earlier step that drops it in another shape has already been
		// refused by dropsObject.
		for j := i + 1; j < len(admitted); j++ {
			again, err := dropsObject(admitted[j].st, d)
			if err != nil {
				return -1, err
			}
			if again {
				return -1, fmt.Errorf("%w: steps %d and %d both drop %s %s, and only one drop is checked",
					ErrInvariantViolation, i+1, j+1, d.Kind, d.Name)
			}
		}
		return i, nil
	}
	return -1, fmt.Errorf("%w: no step drops %s %s, which the plan drops", ErrInvariantViolation, d.Kind, d.Name)
}
