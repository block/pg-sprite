package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// EnumTypeRequest names the inputs to [RunDesiredEnumType]. Zero-value
// fields are invalid: the schema must be set, and the desired state must
// come from [statement.ParseDesiredFile].
type EnumTypeRequest struct {
	// Schema is the schema the enum type lives in.
	Schema string
	// Desired is the parsed desired enum type.
	Desired statement.DesiredEnumType
	// ExpectedFingerprint optionally pins the plan, as
	// DesiredRequest.ExpectedFingerprint does.
	ExpectedFingerprint string
}

// RunDesiredEnumType converges one live enum type onto its desired file:
// derive the plan with [diffplan.PlanEnumType], admit it as a whole, then
// run each planned ALTER TYPE ... ADD VALUE back through the full [Run]
// pipeline, which resolves the type, checks the role may alter it, and
// confirms before each commit that the label reached that type. Each label
// commits on its own; a later refusal or failure leaves the earlier labels
// added, and the verdicts say how far the run got.
//
// A missing type, a name that holds another kind of type, and a file that
// drops or reorders a live label are refused before anything runs, as is a
// plan whose fingerprint is not the pinned one. Convergence is verified,
// not assumed: after the last label commits, the plan is derived again and
// the run is executed only when it is empty.
//
// The result-and-error contract, the rejection of every acknowledgement,
// and the progress reporting are RunDesired's. RunDesiredEnumType does not
// close the pool.
func RunDesiredEnumType(ctx context.Context, pool *pgxpool.Pool, req EnumTypeRequest, opts Options) (result DesiredResult, err error) {
	defer func() { opts.finishProgress(result.Outcome, err) }()
	// INV: RF-12 — desired-state execution takes no acknowledgement.
	if err := rejectDesiredAcks(opts); err != nil {
		return DesiredResult{}, err
	}
	if err := opts.validate(); err != nil {
		return DesiredResult{}, err
	}
	replan := func(ctx context.Context) (plan.Report, error) {
		return diffplan.PlanEnumType(ctx, pool, req.Schema, req.Desired)
	}
	report, err := replan(ctx)
	if ref, ok := desiredPlanRefusal(err); ok {
		return DesiredResult{}.refused(ref, fmt.Sprintf("%v; nothing was planned or executed", err)), nil
	}
	if err != nil {
		return DesiredResult{}, err
	}
	opts.logger().Debug("desired enum type plan derived",
		"schema", report.Schema, "type", report.Type,
		"statements", len(report.Statements), "fingerprint", report.Fingerprint)
	opts.startProgress(len(report.Statements))
	if len(report.Statements) == 0 {
		return DesiredResult{
			Plan:    report,
			Outcome: verdict.OutcomeExecuted,
			Detail:  "already converged: the live enum type carries every label the file lists; nothing to run",
		}, nil
	}
	if refused, ok := admitPlan(DesiredRequest{ExpectedFingerprint: req.ExpectedFingerprint}, report); !ok {
		return refused, nil
	}
	return executePlan(ctx, pool, report, opts, replan)
}
