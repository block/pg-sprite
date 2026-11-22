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

// dropsObject reports whether st is an ALTER TABLE whose only operation
// drops the column, constraint, or NOT NULL that d names. An index drop is
// a DROP INDEX statement and never matches here (see
// ExecuteDropIndexConcurrently).
func dropsObject(st statement.Statement, d *schemadiff.DroppedObject) (bool, error) {
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	if len(ops) != 1 {
		return false, nil
	}
	switch op := ops[0]; d.Kind {
	case schemadiff.DroppedColumn:
		return op.Kind == statement.OpDropColumn && op.Column == d.Name, nil
	case schemadiff.DroppedConstraint:
		return op.Kind == statement.OpDropConstraint && op.Name == d.Name, nil
	case schemadiff.DroppedNotNull:
		return op.Kind == statement.OpDropNotNull && op.Column == d.Name, nil
	default:
		return false, nil
	}
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
		return i, nil
	}
	return -1, fmt.Errorf("%w: no step drops %s %s, which the plan drops", ErrInvariantViolation, d.Kind, d.Name)
}
