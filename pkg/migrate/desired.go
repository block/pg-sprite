package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// stoppedBeforeVerdict is the committed-prefix wording for a statement the
// pipeline stopped on without reaching a verdict: unlike a failed verdict,
// nothing about the statement was executed.
const stoppedBeforeVerdict = "stopped before a verdict; nothing about it was executed"

// DesiredRequest names the inputs to [RunDesired]. Zero-value fields are
// invalid: the schema must be set, and the desired state must come from
// [statement.ParseDesired] — the zero DesiredSchema is refused.
type DesiredRequest struct {
	// Schema is the target schema the desired table lives in.
	Schema string
	// Desired is the parsed desired-state schema for the table.
	Desired statement.DesiredSchema
	// ExpectedFingerprint optionally pins the plan: when set, the plan
	// derived at execution time must carry exactly this fingerprint
	// (plan.Report.Fingerprint) or nothing runs. It is how a caller that
	// had a plan reviewed enforces that the plan the reviewer approved is
	// the plan that executes; empty skips the check.
	ExpectedFingerprint string
}

// DesiredResult is the aggregate report for one desired-state execution:
// the plan that was derived, the verdict of every statement that was
// attempted, and the overall outcome.
//
// Verdicts[i] is the verdict of Plan.Statements[i]; fewer verdicts than
// planned statements means execution stopped and the remaining statements
// were never attempted. The committed prefix is read from the verdicts:
// every executed verdict committed in full, and a failed verdict's own
// ExecutedSQL discloses the committed steps inside the statement that
// failed.
type DesiredResult struct {
	// Plan is the convergence plan derived at execution time; empty
	// Plan.Statements means the live table already matched the desired
	// schema.
	Plan plan.Report `json:"plan"`
	// Verdicts are the per-statement verdicts, in plan order, one per
	// attempted statement.
	Verdicts []verdict.Verdict `json:"verdicts,omitempty"`
	// Outcome is what happened overall: executed when every planned
	// statement committed (or there was nothing to run), refused when the
	// plan or one of its statements was refused and execution stopped,
	// failed when execution stopped on an operational error. A failed
	// result whose stopping statement has no verdict means the pipeline
	// stopped before reaching one — nothing about that statement was
	// executed; a failed verdict means the statement was attempted and
	// failed. Detail says which.
	Outcome verdict.Outcome `json:"outcome"`
	// Reason is the typed refusal cause; empty unless Outcome is refused.
	Reason verdict.Reason `json:"reason,omitempty"`
	// Detail is the human explanation: why refused, what committed, or
	// that there was nothing to do.
	Detail string `json:"detail,omitempty"`
}

// RunDesired converges one live table onto its desired-state schema:
// derive the convergence plan with [diffplan.Plan], admit the plan as a
// whole, then execute each planned statement back through the full [Run]
// pipeline — fresh introspection, classification, and routing per
// statement, so a statement that became unsafe after planning refuses
// instead of running — stopping at the first refusal or failure.
//
// Plan-time admission is all-or-nothing: a plan that needs a table that
// does not exist yet, contains a destructive statement, routes any
// statement away from execution, or does not match the pinned fingerprint
// is refused before anything runs. Execution-time semantics are
// committed-prefix: once statements start running, an executed statement
// stays committed even when a later one refuses or fails, and the result's
// verdicts disclose exactly how far convergence got.
//
// The result-and-error contract mirrors [Run]'s three shapes. A refusal —
// at plan admission or on a mid-plan statement — returns the result with a
// nil error. An execution failure returns the failed result together with
// the operational error. An error with a zero result means the pipeline
// stopped before planning or executing anything.
//
// Options.Force is rejected: the declarative front door never runs a
// submitted form blind. A destructive or force-worthy change belongs on
// the imperative front door where the operator states it explicitly.
//
// RunDesired does not close the pool; one pool serves any number of calls.
func RunDesired(ctx context.Context, pool *pgxpool.Pool, req DesiredRequest, opts Options) (DesiredResult, error) {
	if err := opts.validate(); err != nil {
		return DesiredResult{}, err
	}
	if opts.Force != "" {
		return DesiredResult{}, errors.New(
			"the force acknowledgement applies to the imperative front door only; desired-state execution never runs a submitted form blind")
	}
	report, err := diffplan.Plan(ctx, pool, diffplan.Request{Schema: req.Schema, Desired: req.Desired})
	if err != nil {
		return DesiredResult{}, err
	}
	opts.logger().Debug("desired plan derived",
		"schema", report.Schema, "table", report.Table,
		"statements", len(report.Statements), "fingerprint", report.Fingerprint)

	if refused, ok := admitPlan(req, report); !ok {
		return refused, nil
	}
	if len(report.Statements) == 0 {
		return DesiredResult{
			Plan:    report,
			Outcome: verdict.OutcomeExecuted,
			Detail:  "already converged: the live table matches the desired schema; nothing to run",
		}, nil
	}

	result := DesiredResult{Plan: report}
	for i, ps := range report.Statements {
		st, err := statement.ParseOne(ps.SQL)
		if err != nil {
			// The planned SQL is engine-generated: a reparse failure is a
			// breach of the engine's own contract, not a database problem.
			result.Outcome = verdict.OutcomeFailed
			result.Detail = committedPrefixDetail(i, len(report.Statements), stoppedBeforeVerdict)
			return result, fmt.Errorf("%w: planned statement %d is engine-generated SQL its own parser rejects: %w",
				executor.ErrInvariantViolation, i+1, err)
		}
		v, runErr := Run(ctx, pool, st, opts)
		if runErr != nil {
			result.Outcome = verdict.OutcomeFailed
			// A zero verdict is Run's "stopped before reaching a verdict"
			// shape: nothing about the statement was executed, so the
			// detail must not call the statement failed.
			what := stoppedBeforeVerdict
			if v.Outcome != "" {
				result.Verdicts = append(result.Verdicts, v)
				what = "failed"
			}
			result.Detail = committedPrefixDetail(i, len(report.Statements), what)
			return result, fmt.Errorf("planned statement %d: %w", i+1, runErr)
		}
		result.Verdicts = append(result.Verdicts, v)
		if v.Outcome == verdict.OutcomeRefused {
			result.Outcome = verdict.OutcomeRefused
			result.Reason = v.Reason
			result.Detail = committedPrefixDetail(i, len(report.Statements), "was refused at execution time")
			return result, nil
		}
	}
	result.Outcome = verdict.OutcomeExecuted
	result.Detail = fmt.Sprintf("converged: all %d planned statements committed", len(report.Statements))
	return result, nil
}

// admitPlan is the all-or-nothing plan-time admission: it refuses the whole
// plan — before anything runs — when the plan cannot converge the table as
// a unit. The checks run from the caller's contract outward: the pinned
// fingerprint first (the caller's approval is void whatever else holds),
// then the table's existence, then the destructive guard, then the routed
// dispositions.
func admitPlan(req DesiredRequest, report plan.Report) (DesiredResult, bool) {
	refused := DesiredResult{Plan: report, Outcome: verdict.OutcomeRefused}
	if req.ExpectedFingerprint != "" && req.ExpectedFingerprint != report.Fingerprint {
		refused.Reason = verdict.ReasonPlanFingerprintMismatch
		refused.Detail = fmt.Sprintf(
			"the plan derived at execution time (fingerprint %s) is not the pinned plan (fingerprint %s); "+
				"the live table or the desired schema changed since the plan was reviewed — re-review the new plan",
			report.Fingerprint, req.ExpectedFingerprint)
		return refused, false
	}
	if report.TableExists != nil && !*report.TableExists {
		refused.Reason = verdict.ReasonUnsupportedStatement
		refused.Detail = fmt.Sprintf(
			"table %s.%s does not exist; desired-state execution converges an existing table — "+
				"create the table from the plan's SQL script first",
			report.Schema, report.Table)
		return refused, false
	}
	for i, ps := range report.Statements {
		if !ps.Destructive {
			continue
		}
		refused.Reason = verdict.ReasonDestructiveChange
		// The deliberate path differs by shape: the imperative front door
		// runs an ALTER TABLE drop when the operator states it, but it
		// refuses a plain DROP INDEX in favor of the concurrent idiom — so
		// an index drop is pointed straight at that idiom instead of at a
		// door that would bounce it.
		deliberatePath := "run it deliberately through the imperative front door"
		if ps.Kind == schemadiff.ChangeDropIndex {
			deliberatePath = "drop it deliberately with DROP INDEX CONCURRENTLY, then rerun"
		}
		refused.Detail = fmt.Sprintf(
			"planned statement %d discards live structure (%s); desired-state execution runs no "+
				"destructive statement — %s",
			i+1, ps.SQL, deliberatePath)
		return refused, false
	}
	if report.Disposition != router.DispositionExecute {
		refused.Reason, refused.Detail = planRefusal(report)
		return refused, false
	}
	return DesiredResult{}, true
}

// planRefusal maps the first non-executable planned statement to the typed
// refusal the aggregate result carries, mirroring how [Run] refuses the
// same dispositions at execution time.
func planRefusal(report plan.Report) (verdict.Reason, string) {
	for i, ps := range report.Statements {
		detail := func(why string) string {
			return fmt.Sprintf("planned statement %d (%s) %s; nothing was executed", i+1, ps.SQL, why)
		}
		switch ps.Disposition {
		case router.DispositionExecute:
			continue
		case router.DispositionRewriteRequired:
			return verdict.ReasonRewriteRequired, detail("blocks and has no safer native sequence")
		case router.DispositionUnavailable:
			return verdict.ReasonBackendUnavailable, detail("routes to an execution strategy this build does not implement")
		case router.DispositionRefuse:
			reason := ps.Reason
			if reason == verdict.ReasonNone {
				reason = verdict.ReasonUnsupportedStatement
			}
			return reason, detail("has no safe path")
		default:
			return verdict.ReasonUnsupportedStatement, detail("carries a disposition this build does not know")
		}
	}
	// The aggregate disposition is non-executable but every statement is:
	// a report this build cannot have produced. Refuse rather than guess.
	return verdict.ReasonUnsupportedStatement,
		fmt.Sprintf("the plan's aggregate disposition is %q but no statement carries it; nothing was executed",
			report.Disposition)
}

// committedPrefixDetail states how far convergence got when execution
// stopped at statement i (0-based) of n: the statements before it are
// committed and stay committed.
func committedPrefixDetail(i, n int, what string) string {
	return fmt.Sprintf("planned statement %d of %d %s; the %d preceding statements committed and remain in effect",
		i+1, n, what, i)
}
