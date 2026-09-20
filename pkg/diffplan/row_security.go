package diffplan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// PlanWithRowSecurity verifies an explicitly RLS-scoped desired definition.
// Only an unchanged existing table produces a report, with no executable steps.
// Missing tables and any delta return ErrUnsupportedChange; the distinct parsed
// type cannot be passed to RunDesired or ExecuteCreate. Inspection still needs
// CREATE privilege for its rolled-back scratch schema, just like Plan.
func PlanWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity) (plan.Report, error) {
	if schema == "" {
		return plan.Report{}, errors.New("plan desired row security: schema name is required")
	}
	if desired.Table() == "" {
		return plan.Report{}, statement.ErrRowSecurityDeclaration
	}
	live, err := schemadiff.Introspect(ctx, pool, schema, desired.Table())
	if errors.Is(err, schemadiff.ErrTableNotFound) {
		return plan.Report{}, fmt.Errorf("creating tables with managed row security is not supported: %w: %w", schemadiff.ErrUnsupportedChange, err)
	}
	if err != nil {
		return plan.Report{}, err
	}
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(ctx, pool, desired)
	if err != nil {
		return plan.Report{}, err
	}
	if _, err := schemadiff.DiffWithRowSecurity(schema, live, model); err != nil {
		return plan.Report{}, err
	}
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table = schema, desired.Table()
	exists := true
	report.TableExists = &exists
	report.Disposition = router.DispositionExecute
	report.Fingerprint = plan.Fingerprint(report.Statements)
	report.ServerVersion, err = dbconn.ServerVersion(ctx, pool)
	if err != nil {
		return plan.Report{}, err
	}
	return report, nil
}
