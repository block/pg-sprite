package diffplan

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// recordNotNullScaffolds records the helper of each planned SET NOT NULL that
// runs as the online sequence: ADD CONSTRAINT of a helper CHECK (column IS
// NOT NULL) NOT VALID, its VALIDATE CONSTRAINT, SET NOT NULL, and DROP
// CONSTRAINT.
//
// Validates gets the helper with the definition its add leaves, as any
// constraint the plan adds NOT VALID does. The validation step then checks
// that the helper still has that definition, so a validation that resumes
// after a failure never validates a constraint someone else put under the
// helper's name. It also lets a caller that gates validations on the plan's
// record see that this one validates the plan's own helper.
//
// DropsHelper gets the helper with the definition it has once validated, when
// a later step of the sequence drops it. A caller that runs each step on its
// own checks the drop against it, as it checks any drop the plan records.
//
// Both definitions are the ones pg_get_constraintdef prints for the helper.
// PostgreSQL deparses the column reference with the same quoting rule as
// quote_ident, which the server applies here so a keyword or mixed-case
// column name renders exactly as the catalog will.
func recordNotNullScaffolds(ctx context.Context, pool *pgxpool.Pool, statements []plan.Statement) error {
	for i := range statements {
		ps := &statements[i]
		if ps.Validates != nil {
			continue
		}
		scaffold, ok, err := notNullScaffold(*ps)
		if err != nil {
			return fmt.Errorf("plan derived statement %q: %w", ps.SQL, err)
		}
		if !ok {
			continue
		}
		var quoted string
		if err := pool.QueryRow(ctx, "SELECT pg_catalog.quote_ident($1)", scaffold.column).Scan(&quoted); err != nil {
			return fmt.Errorf("quote column %s for the definition of helper constraint %s: %w", scaffold.column, scaffold.constraint, err)
		}
		validated := "CHECK ((" + quoted + " IS NOT NULL))"
		ps.Validates = &schemadiff.ConstraintValidation{
			Constraint: scaffold.constraint,
			Def:        validated + " NOT VALID",
		}
		if scaffold.dropped {
			ps.DropsHelper = &schemadiff.DroppedObject{
				Kind:       schemadiff.DroppedConstraint,
				Name:       scaffold.constraint,
				Definition: validated,
			}
		}
	}
	return nil
}

// notNullHelper is the helper constraint of a planned SET NOT NULL, the
// column it checks, and whether a later step of the sequence drops it.
type notNullHelper struct {
	constraint string
	column     string
	dropped    bool
}

// notNullScaffold returns the helper of a planned SET NOT NULL whose first two
// steps add that helper as a NOT VALID CHECK and then validate it. ok is false
// for any other statement.
func notNullScaffold(ps plan.Statement) (helper notNullHelper, ok bool, err error) {
	source, err := singleOp(ps.SQL)
	if err != nil {
		return notNullHelper{}, false, err
	}
	if !isSetNotNull(source) || len(ps.ExecSQL) < 2 {
		return notNullHelper{}, false, nil
	}
	add, err := singleOp(ps.ExecSQL[0])
	if err != nil {
		return notNullHelper{}, false, err
	}
	if !isNotValidCheckAdd(add) {
		return notNullHelper{}, false, nil
	}
	validate, err := singleOp(ps.ExecSQL[1])
	if err != nil {
		return notNullHelper{}, false, err
	}
	if !validatesConstraint(validate, add.Name) {
		return notNullHelper{}, false, nil
	}
	helper = notNullHelper{constraint: add.Name, column: source.Column}
	for _, sql := range ps.ExecSQL[2:] {
		step, err := singleOp(sql)
		if err != nil {
			return notNullHelper{}, false, err
		}
		if dropsConstraint(step, add.Name) {
			helper.dropped = true
		}
	}
	return helper, true, nil
}

// isSetNotNull reports whether op is an ALTER COLUMN ... SET NOT NULL.
func isSetNotNull(op *statement.Op) bool {
	return op != nil && op.Kind == statement.OpSetNotNull
}

// isNotValidCheckAdd reports whether op adds a CHECK constraint NOT VALID.
func isNotValidCheckAdd(op *statement.Op) bool {
	if op == nil || op.Kind != statement.OpAddConstraint {
		return false
	}
	return op.Constraint == statement.ConstraintCheck && op.NotValid
}

// validatesConstraint reports whether op validates the constraint named name.
func validatesConstraint(op *statement.Op, name string) bool {
	return op != nil && op.Kind == statement.OpValidateConstraint && op.Name == name
}

// dropsConstraint reports whether op drops the constraint named name, without
// CASCADE.
func dropsConstraint(op *statement.Op, name string) bool {
	return op != nil && op.Kind == statement.OpDropConstraint && op.Name == name && !op.Cascade
}

// singleOp returns the one operation sql carries, or nil when it carries
// more or fewer.
func singleOp(sql string) (*statement.Op, error) {
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return nil, fmt.Errorf("read the operations of %q: %w", sql, err)
	}
	if len(ops) != 1 {
		return nil, nil
	}
	return &ops[0], nil
}
