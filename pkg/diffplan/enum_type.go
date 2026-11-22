package diffplan

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// PlanEnumType derives the routed convergence plan for a desired enum type
// against the live type of the same name in schema: an ALTER TYPE ... ADD
// VALUE for each label the type lacks, in declared order
// (schemadiff.DiffEnumType), classified and routed like every planned
// statement. Adding a label locks only the type, never a table that uses
// it. The report names the type in Type and has no Table; an empty plan
// means the type matches.
//
// A missing type, a name that holds another kind of type, and a file that
// drops or reorders a live label return the *schemadiff.UnsupportedChangeError
// for the cause, and nothing is planned. Unlike Plan, PlanEnumType only
// reads the catalog, so a read-only connection serves it. It does not close
// the pool.
func PlanEnumType(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredEnumType) (plan.Report, error) {
	if schema == "" {
		return plan.Report{}, errors.New("plan desired enum type: schema name is required")
	}
	if desired.Name() == "" {
		return plan.Report{}, errors.New("plan desired enum type: desired state names no type")
	}
	live, err := schemadiff.IntrospectEnumType(ctx, pool, schema, desired.Name())
	if err != nil {
		return plan.Report{}, err
	}
	changes, err := schemadiff.DiffEnumType(live, desired)
	if err != nil {
		return plan.Report{}, err
	}
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Type = schema, desired.Name()
	if report.ServerVersion, err = dbconn.ServerVersion(ctx, pool); err != nil {
		return plan.Report{}, err
	}
	if report.Statements, report.Disposition, err = classifyChanges(changes, planner.Facts{}); err != nil {
		return plan.Report{}, err
	}
	report.Fingerprint = report.ComputeFingerprint()
	return report, nil
}
