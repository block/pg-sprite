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
// Deltas return RowSecurityReviewRequired, wrapping ErrUnsupportedChange. Missing
// tables remain unsupported without a review. The distinct parsed
// type cannot be passed to RunDesired or ExecuteCreate. Inspection still needs
// CREATE privilege for its rolled-back scratch schema, just like Plan.
func PlanWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity) (plan.Report, error) {
	live, model, err := inspectRowSecurity(ctx, pool, schema, desired)
	if err != nil {
		return plan.Report{}, err
	}
	if _, err := schemadiff.DiffWithRowSecurity(schema, live, model); err != nil {
		review, reviewErr := schemadiff.ReviewRowSecurity(schema, live, model)
		if reviewErr != nil {
			return plan.Report{}, reviewErr
		}
		return plan.Report{}, &RowSecurityReviewRequired{Schema: schema, Table: desired.Table(), Review: review, cause: err}
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

// RowSecurityReviewRequired carries review-only changes while preserving the
// unsupported-change error boundary. It never authorizes execution.
type RowSecurityReviewRequired struct {
	Schema string
	Table  string
	Review schemadiff.RowSecurityReview
	// ReviewMatches is present only for an explicit freshness check.
	ReviewMatches *bool
	cause         error
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

// inspectRowSecurity captures definitions for diagnostics only. These reads do
// not hold a lock across review and must never be reused as execution admission.
func inspectRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity) (schemadiff.Model, schemadiff.Model, error) {
	if schema == "" {
		return schemadiff.Model{}, schemadiff.Model{}, errors.New("plan desired row security: schema name is required")
	}
	if desired.Table() == "" {
		return schemadiff.Model{}, schemadiff.Model{}, statement.ErrRowSecurityDeclaration
	}
	live, err := schemadiff.Introspect(ctx, pool, schema, desired.Table())
	if errors.Is(err, schemadiff.ErrTableNotFound) {
		return schemadiff.Model{}, schemadiff.Model{}, fmt.Errorf("creating tables with managed row security is not supported: %w: %w", schemadiff.ErrUnsupportedChange, err)
	}
	if err != nil {
		return schemadiff.Model{}, schemadiff.Model{}, err
	}
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(ctx, pool, desired)
	if err != nil {
		return schemadiff.Model{}, schemadiff.Model{}, err
	}
	return live, model, nil
}

// VerifyRowSecurityReview freshly inspects live and desired definitions and
// compares their identity with expected. A match returns diagnostic information,
// never an executable plan. Database identity and external access dependencies
// must be tracked separately by the caller; this check holds no execution lock.
func VerifyRowSecurityReview(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, expected string) (schemadiff.RowSecurityReview, error) {
	if err := schemadiff.ValidateRowSecurityFingerprint(expected); err != nil {
		return schemadiff.RowSecurityReview{}, err
	}
	live, model, err := inspectRowSecurity(ctx, pool, schema, desired)
	if err != nil {
		return schemadiff.RowSecurityReview{}, err
	}
	review, err := schemadiff.ReviewRowSecurity(schema, live, model)
	if err != nil {
		return schemadiff.RowSecurityReview{}, err
	}
	return review, review.CheckFingerprint(expected)
}
