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

// PlanWithRowSecurity plans an explicitly RLS-scoped desired definition. An
// unchanged existing table produces a report with no executable steps. When
// the live row security matches the file and only the table definition
// differs, the report is the plan Plan derives for the file's table
// definition: it changes the table and never its row security, and
// migrate.RunDesiredWithRowSecurity runs it; a table change that plan
// refuses returns its error marked with schemadiff.ErrOnlyTableDiffers. The
// report is empty only when one comparison of the whole file, row security
// included, finds it matching. A row security delta, alone or with table
// changes, returns RowSecurityReviewRequired, wrapping ErrUnsupportedChange.
// A missing table
// returns schemadiff.ErrTableNotFound; preview its creation with
// executor.PreviewDesiredCreateWithRowSecurity. The distinct parsed
// type cannot be passed to RunDesired or ExecuteCreate. Inspection still needs
// CREATE privilege for its rolled-back scratch schema, just like Plan.
func PlanWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity) (plan.Report, error) {
	return planWithRowSecurity(ctx, pool, schema, desired, nil)
}

// planWithRowSecurity is PlanWithRowSecurity. beforeTablePlan, when set,
// runs after the comparison finds only the table definition differing and
// before that definition is planned, the window in which another session
// can change the table.
func planWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, beforeTablePlan func(context.Context) error) (plan.Report, error) {
	if schema == "" {
		return plan.Report{}, errors.New("plan desired row security: schema name is required")
	}
	if desired.Table() == "" {
		return plan.Report{}, statement.ErrRowSecurityDeclaration
	}
	err := compareWithRowSecurity(ctx, pool, schema, desired)
	if errors.Is(err, schemadiff.ErrOnlyTableDiffers) {
		if beforeTablePlan != nil {
			if err := beforeTablePlan(ctx); err != nil {
				return plan.Report{}, err
			}
		}
		report, err := planTableDefinition(ctx, pool, schema, desired)
		if err != nil || len(report.Statements) > 0 {
			return report, err
		}
		// The comparison found the table differing and its plan found it
		// matching, so another session changed the table between the two
		// reads, and may have changed the row security too. The file is
		// unchanged only if one comparison of the whole file now says so.
		if err := compareWithRowSecurity(ctx, pool, schema, desired); err != nil {
			return plan.Report{}, fmt.Errorf("%s.%s changed while it was planned; plan it again: %w", schema, desired.Table(), err)
		}
		return unchangedReport(ctx, pool, schema, desired)
	}
	if err != nil {
		return plan.Report{}, err
	}
	return unchangedReport(ctx, pool, schema, desired)
}

// compareWithRowSecurity compares the whole file, table definition and row
// security, against one read of the live table. It returns nil when both
// match, an error wrapping schemadiff.ErrOnlyTableDiffers when only the
// table definition differs, and RowSecurityReviewRequired when the row
// security differs.
func compareWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity) error {
	live, err := schemadiff.Introspect(ctx, pool, schema, desired.Table())
	if err != nil {
		return err
	}
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(ctx, pool, schema, desired)
	if err != nil {
		return err
	}
	_, err = schemadiff.DiffWithRowSecurity(schema, live, model)
	if err == nil || errors.Is(err, schemadiff.ErrOnlyTableDiffers) {
		return err
	}
	review, reviewErr := schemadiff.ReviewRowSecurity(schema, live, model)
	if reviewErr != nil {
		return reviewErr
	}
	return &RowSecurityReviewRequired{Schema: schema, Table: desired.Table(), Review: review, cause: err}
}

// unchangedReport is the report for an existing table that matches the
// whole file: no executable steps.
func unchangedReport(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity) (plan.Report, error) {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table = schema, desired.Table()
	exists := true
	report.TableExists = &exists
	report.Disposition = router.DispositionExecute
	report.Fingerprint = report.ComputeFingerprint()
	var err error
	report.ServerVersion, err = dbconn.ServerVersion(ctx, pool)
	if err != nil {
		return plan.Report{}, err
	}
	return report, nil
}

// planTableDefinition plans a row-security file's table definition on its
// own, once the row security was found to match. Planning reads the table
// again, so another session can drop it in between, and the plan would then
// create the table without its row security. That plan is refused as a
// missing table, which the caller previews and creates with its row
// security instead. A table change the plan refuses keeps its own error,
// marked with schemadiff.ErrOnlyTableDiffers so the caller knows the row
// security matched.
func planTableDefinition(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity) (plan.Report, error) {
	report, err := Plan(ctx, pool, Request{Schema: schema, Desired: desired.TableDefinition()})
	if errors.Is(err, schemadiff.ErrUnsupportedChange) {
		return plan.Report{}, fmt.Errorf("%w: %w", schemadiff.ErrOnlyTableDiffers, err)
	}
	if err != nil {
		return plan.Report{}, err
	}
	if report.TableExists != nil && !*report.TableExists {
		return plan.Report{}, fmt.Errorf("%s.%s was dropped while its table definition was planned: %w", schema, desired.Table(), schemadiff.ErrTableNotFound)
	}
	return report, nil
}

// RowSecurityReviewRequired carries review-only changes while preserving the
// unsupported-change error boundary. It never authorizes execution.
type RowSecurityReviewRequired struct {
	Schema string
	Table  string
	Review schemadiff.RowSecurityReview
	cause  error
}

// Error explains why the comparison cannot produce an executable plan.
func (e *RowSecurityReviewRequired) Error() string {
	if e.cause == nil {
		return "row security review required"
	}
	return e.cause.Error()
}

// Unwrap preserves the existing typed refusal.
func (e *RowSecurityReviewRequired) Unwrap() error { return e.cause }
