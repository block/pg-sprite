package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrValidatedConstraintDrift means the constraint a planned VALIDATE
// CONSTRAINT validates no longer has the definition the plan matched it
// against, read again under the table's lock: it was dropped, or replaced
// under the same name. Nothing was validated. The outcome is permanent:
// re-plan against the constraint as it stands.
var ErrValidatedConstraintDrift = errors.New("the constraint the validation checks changed after planning")

// validatesConstraint reports whether st is an ALTER TABLE whose only
// operation is VALIDATE CONSTRAINT of the constraint named name.
func validatesConstraint(st statement.Statement, name string) (bool, error) {
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	return len(ops) == 1 && ops[0].Kind == statement.OpValidateConstraint && ops[0].Name == name, nil
}

// refuseValidatedConstraintDrift locks st's table in SHARE UPDATE
// EXCLUSIVE, the lock the VALIDATE CONSTRAINT takes anyway and one that
// blocks every DROP, ADD, and RENAME CONSTRAINT on the table without
// blocking reads or writes, then reads the constraint the validation
// checks and refuses unless it still has the definition v. Holding the
// lock until the validation commits means the constraint cannot change
// between this read and the validation. The read runs under the attempt's
// search_path, the one introspection reads under, so the two definitions
// print alike. A constraint someone validated since planning, with the
// same definition otherwise, passes: validating it again changes nothing.
func refuseValidatedConstraintDrift(ctx context.Context, tx pgx.Tx, st statement.Statement, v schemadiff.ConstraintValidation) error {
	target, err := lockTableForCheck(ctx, tx, st, "SHARE UPDATE EXCLUSIVE", "check the constraint it validates")
	if err != nil {
		return err
	}
	var def string
	err = tx.QueryRow(ctx, `SELECT pg_catalog.pg_get_constraintdef(oid)
		FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND conname = $2`, target, v.Constraint).Scan(&def)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s has no constraint named %s any more; re-plan", ErrValidatedConstraintDrift, target, v.Constraint)
	case err != nil:
		return fmt.Errorf("read constraint %s on %s: %w", v.Constraint, target, err)
	}
	if !slices.Contains([]string{v.Def, strings.TrimSuffix(v.Def, " NOT VALID")}, def) {
		return fmt.Errorf("%w: constraint %s on %s is now %q, not the %q the plan matched; re-plan",
			ErrValidatedConstraintDrift, v.Constraint, target, def, v.Def)
	}
	return nil
}

// validationStep returns the index of the admitted step that validates
// validates.Constraint, or -1 when validates is nil. A planned validation
// no step performs, or one a step other than a validate step performs, is
// a breach of the plan's own contract and refuses before anything runs.
func validationStep(admitted []sequenceStep, validates *schemadiff.ConstraintValidation) (int, error) {
	if validates == nil {
		return -1, nil
	}
	for i, step := range admitted {
		ok, err := validatesConstraint(step.st, validates.Constraint)
		if err != nil {
			return -1, err
		}
		if !ok {
			continue
		}
		if step.kind != StepValidateConstraint {
			return -1, fmt.Errorf("%w: step %d validates constraint %s as a %s step, not a validate one",
				ErrInvariantViolation, i+1, validates.Constraint, step.kind)
		}
		return i, nil
	}
	return -1, fmt.Errorf("%w: no step validates constraint %s, which the plan validates", ErrInvariantViolation, validates.Constraint)
}
