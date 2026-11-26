package diffplan

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
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
// steps add that helper as a NOT VALID CHECK and then validate it, or whose
// first step validates a helper an earlier run left on the live table
// (resumeNotNullHelpers). ok is false for any other statement.
func notNullScaffold(ps plan.Statement) (helper notNullHelper, ok bool, err error) {
	source, err := singleOp(ps.SQL)
	if err != nil {
		return notNullHelper{}, false, err
	}
	if !isSetNotNull(source) || len(ps.ExecSQL) < 2 {
		return notNullHelper{}, false, nil
	}
	first, err := singleOp(ps.ExecSQL[0])
	if err != nil {
		return notNullHelper{}, false, err
	}
	rest := ps.ExecSQL[1:]
	switch {
	case isNotValidCheckAdd(first):
		validate, err := singleOp(ps.ExecSQL[1])
		if err != nil {
			return notNullHelper{}, false, err
		}
		if !validatesConstraint(validate, first.Name) {
			return notNullHelper{}, false, nil
		}
		rest = ps.ExecSQL[2:]
	case first != nil && first.Kind == statement.OpValidateConstraint:
	default:
		return notNullHelper{}, false, nil
	}
	helper = notNullHelper{constraint: first.Name, column: source.Column}
	for _, sql := range rest {
		step, err := singleOp(sql)
		if err != nil {
			return notNullHelper{}, false, err
		}
		if dropsConstraint(step, first.Name) {
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

// resumeNotNullHelpers finds, for each SET NOT NULL the diff plans, the helper
// CHECK an earlier run of its online sequence left on the live table: a run
// stopped after adding the helper and before dropping it, because its
// validation failed and the run was cancelled or gave up. The diff reads that
// helper as a live constraint the desired schema lacks and plans its drop, a
// drop of live structure a reviewer would have to approve, followed by a
// sequence that adds the same helper again.
//
// A live constraint is that helper only when it has the helper's name and
// exactly the definition the sequence gives it, NOT VALID or validated. Its
// drop leaves the changes, and the column is returned so the planner resumes
// the sequence at the helper's validation; the sequence's own last step still
// drops it. Any other constraint under that name keeps its drop.
func resumeNotNullHelpers(ctx context.Context, pool *pgxpool.Pool, table string, changes []schemadiff.Change) ([]schemadiff.Change, map[string]bool, error) {
	var helpers map[string]bool
	for _, ch := range changes {
		op, err := singleOp(ch.SQL)
		if err != nil {
			return nil, nil, fmt.Errorf("plan derived statement %q: %w", ch.SQL, err)
		}
		if !isSetNotNull(op) {
			continue
		}
		name := planner.NotNullHelperName(table, op.Column)
		var quoted string
		if err := pool.QueryRow(ctx, "SELECT pg_catalog.quote_ident($1)", op.Column).Scan(&quoted); err != nil {
			return nil, nil, fmt.Errorf("quote column %s for the definition of helper constraint %s: %w", op.Column, name, err)
		}
		validated := "CHECK ((" + quoted + " IS NOT NULL))"
		drop := slices.IndexFunc(changes, func(other schemadiff.Change) bool {
			return other.Drops != nil && other.Drops.Kind == schemadiff.DroppedConstraint && other.Drops.Name == name &&
				(other.Drops.Definition == validated || other.Drops.Definition == validated+" NOT VALID")
		})
		if drop < 0 {
			continue
		}
		changes = slices.Delete(slices.Clone(changes), drop, drop+1)
		if helpers == nil {
			helpers = make(map[string]bool)
		}
		helpers[op.Column] = true
	}
	return changes, helpers, nil
}

// LeftoverNotNullHelpers returns each helper CHECK that a stopped run of the
// online SET NOT NULL sequence left on schema.table, as the record a checked
// drop of it needs. A run leaves one when it stops after adding the helper and
// before dropping it: its validation failed on a NULL row and the run was
// cancelled or gave up. Until it is dropped, the helper rejects every write
// that leaves its column NULL.
//
// A constraint is such a helper under the same rule the planner uses to
// resume one: the name the sequence gives the helper of one of the table's
// columns, and exactly the definition the sequence gives it, NOT VALID or
// validated. PostgreSQL never generates that name itself. The records are
// read from the live catalog, so a drop checked against one refuses a
// constraint replaced after the read.
func LeftoverNotNullHelpers(ctx context.Context, pool *pgxpool.Pool, schema, table string) ([]schemadiff.DroppedObject, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.conname, pg_catalog.pg_get_constraintdef(c.oid), a.attname, pg_catalog.quote_ident(a.attname)
		FROM pg_catalog.pg_constraint c
		JOIN pg_catalog.pg_class r ON r.oid = c.conrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = r.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = r.oid AND a.attnum > 0 AND NOT a.attisdropped
		WHERE n.nspname = $1 AND r.relname = $2 AND c.contype = 'c'
		ORDER BY c.conname, a.attnum`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("read the check constraints of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var helpers []schemadiff.DroppedObject
	for rows.Next() {
		var name, def, column, quoted string
		if err := rows.Scan(&name, &def, &column, &quoted); err != nil {
			return nil, fmt.Errorf("read the check constraints of %s.%s: %w", schema, table, err)
		}
		if name != planner.NotNullHelperName(table, column) {
			continue
		}
		validated := "CHECK ((" + quoted + " IS NOT NULL))"
		if def != validated && def != validated+" NOT VALID" {
			continue
		}
		helpers = append(helpers, schemadiff.DroppedObject{Kind: schemadiff.DroppedConstraint, Name: name, Definition: def})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the check constraints of %s.%s: %w", schema, table, err)
	}
	return helpers, nil
}
