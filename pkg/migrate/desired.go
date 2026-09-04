package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// stoppedBeforeVerdict is the committed-prefix wording for a statement the
// pipeline stopped on without reaching a verdict: unlike a failed verdict,
// nothing about the statement was executed.
const stoppedBeforeVerdict = "stopped before a verdict; nothing about it was executed"

// ErrForceNotSupported is returned by [RunDesired] when Options.Force is
// set: the force acknowledgement applies to the imperative front door
// only — desired-state execution never runs a submitted form blind. It is
// a sentinel so an embedder can tell the unsupported option apart from an
// operational failure with [errors.Is].
var ErrForceNotSupported = errors.New(
	"the force acknowledgement applies to the imperative front door only; desired-state execution never runs a submitted form blind")

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
// failed. Whether anything changed is read from the plan: an executed
// outcome with empty Plan.Statements is the no-op signal — the live table
// already matched the desired schema and nothing ran.
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
// A table that does not exist yet takes the greenfield create path
// instead: the plan is the desired schema itself, and after the same
// whole-plan admission the executor's create path verifies the name is
// free and the role can create in the schema, then runs the CREATE TABLE
// and the index builds as brief bounded steps. An occupied name is a typed
// [verdict.ReasonCreateCollision] refusal — the caller re-derives the plan
// against the live catalog rather than assuming the occupant's shape.
//
// Plan-time admission is all-or-nothing: a plan that contains a
// destructive statement, routes any statement away from execution, or
// does not match the pinned fingerprint is refused before anything runs.
// Execution-time semantics are committed-prefix: once statements start
// running, an executed statement stays committed even when a later one
// refuses or fails, and the result's verdicts disclose exactly how far
// convergence got.
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
		return DesiredResult{}, ErrForceNotSupported
	}
	report, err := diffplan.Plan(ctx, pool, diffplan.Request{Schema: req.Schema, Desired: req.Desired})
	if err != nil {
		return DesiredResult{}, err
	}
	opts.logger().Debug("desired plan derived",
		"schema", report.Schema, "table", report.Table,
		"statements", len(report.Statements), "fingerprint", report.Fingerprint)

	// An already-converged table resolves before admission: an empty plan
	// carries no plan identity for a pinned fingerprint to verify (every
	// empty plan hashes alike), and nothing runs either way — which is all
	// a pin protects. Checking the pin first would refuse a legitimate
	// re-run of an approved plan that already converged.
	if len(report.Statements) == 0 {
		return DesiredResult{
			Plan:    report,
			Outcome: verdict.OutcomeExecuted,
			Detail:  "already converged: the live table matches the desired schema; nothing to run",
		}, nil
	}
	if refused, ok := admitPlan(req, report); !ok {
		return refused, nil
	}
	if report.TableExists != nil && !*report.TableExists {
		// The table does not exist: the plan is the desired schema itself
		// and converging it means creating the table. The create path runs
		// the whole plan through the executor's greenfield sequence — the
		// per-statement Run pipeline below states facts about an existing
		// table and its gate refuses CREATE TABLE outright.
		return runCreate(ctx, pool, req, report, opts)
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

// runCreate is the greenfield branch of desired-state execution: the plan's
// table does not exist, so converging it means creating it. The absence and
// creation-access proofs are minted here — in the session that executes, at
// the point of use — and the executor's create path runs the CREATE TABLE
// first and then the index builds, each as one brief bounded step.
//
// The result mirrors the convergence loop's shapes. An occupied target name
// or a missing creation grant is a whole-plan refusal — nothing has
// executed. Once steps start committing, semantics are committed-prefix: a
// created table stays created when a later index build fails, the verdicts
// disclose exactly how far the create got, and a rerun re-derives the plan
// against the live catalog — which now sees the table — and converges the
// remainder through the alter loop.
func runCreate(ctx context.Context, pool *pgxpool.Pool, req DesiredRequest, report plan.Report, opts Options) (DesiredResult, error) {
	result := DesiredResult{Plan: report}
	stopBefore := func(err error) (DesiredResult, error) {
		result.Outcome = verdict.OutcomeFailed
		result.Detail = committedPrefixDetail(0, len(report.Statements), stoppedBeforeVerdict)
		return result, err
	}
	at, err := preflight.CheckTableAbsent(ctx, pool, req.Schema, report.Table)
	if preflight.IsNameOccupied(err) {
		result.Outcome = verdict.OutcomeRefused
		result.Reason = verdict.ReasonCreateCollision
		result.Detail = fmt.Sprintf(
			"the plan creates %s.%s but the name is already occupied (%v); the live catalog changed "+
				"since the plan was derived — re-derive the plan and review what it says now; nothing was executed",
			report.Schema, report.Table, err)
		return result, nil
	}
	if err != nil {
		return stopBefore(fmt.Errorf("verify %s.%s is absent: %w", report.Schema, report.Table, err))
	}
	role, err := preflight.CheckCreatePrivileges(ctx, pool, req.Schema)
	var privErr *preflight.PrivilegeError
	if errors.As(err, &privErr) {
		result.Outcome = verdict.OutcomeRefused
		result.Reason = verdict.ReasonInsufficientPrivileges
		result.Detail = privErr.Error() + "; nothing was executed"
		return result, nil
	}
	if err != nil {
		return stopBefore(fmt.Errorf("verify creation access in schema %s: %w", report.Schema, err))
	}
	opts.logger().Debug("create preflight passed",
		"schema", at.Schema(), "table", at.Table(), "role", role.Role())

	rep, execErr := executor.ExecuteCreate(ctx, pool, at, role, req.Desired, opts.Budget.Brief, opts.retry())
	// The plan's statements and the executor's steps share one order — the
	// CREATE TABLE first, then the indexes in input order — so the verdict
	// at position i is the verdict of Plan.Statements[i].
	for i := range rep.Steps {
		result.Verdicts = append(result.Verdicts, createStepVerdict(report, i, opts))
	}
	if execErr == nil {
		result.Outcome = verdict.OutcomeExecuted
		// The count comes from the committed steps — the same source as
		// the verdicts above — so the disclosure cannot claim more than
		// the executor reported committing.
		result.Detail = fmt.Sprintf("created: all %d planned statements committed", len(rep.Steps))
		return result, nil
	}
	var stepErr *executor.SequenceStepError
	if !errors.As(execErr, &stepErr) {
		// No step error means nothing started: the executor refused the
		// set at admission, from the statements' shapes alone.
		if isCreateAdmissionRefusal(execErr) {
			result.Outcome = verdict.OutcomeRefused
			result.Reason = verdict.ReasonUnsupportedStatement
			result.Detail = fmt.Sprintf("the create path refused the plan: %v; nothing was executed", execErr)
			return result, nil
		}
		return stopBefore(fmt.Errorf("create %s.%s: %w", report.Schema, report.Table, execErr))
	}
	failed := verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.OutcomeCode(execErr)),
		Statement: planStatementSQL(report, stepErr.Step-1),
		Table:     report.Schema + "." + report.Table,
		Detail:    "the step's bounded attempt failed and rolled back; Code names the outcome",
	}
	result.Verdicts = append(result.Verdicts, failed)
	result.Outcome = verdict.OutcomeFailed
	result.Detail = committedPrefixDetail(stepErr.Step-1, len(report.Statements), "failed")
	return result, fmt.Errorf("planned statement %d: %w", stepErr.Step, execErr)
}

// createStepVerdict renders one committed create-path step as the executed
// verdict of the plan statement at the same position.
func createStepVerdict(report plan.Report, i int, opts Options) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: planStatementSQL(report, i),
		Table:     report.Schema + "." + report.Table,
		Detail: fmt.Sprintf("committed within budgets (lock %s, statement %s): the change was effectively instant",
			opts.Budget.Brief.LockTimeout, opts.Budget.Brief.StatementTimeout),
	}
}

// planStatementSQL returns the plan statement at i, empty when the position
// is out of range — a defensive read: the executor's step count equals the
// plan's statement count by construction, and a mismatch must not panic a
// result renderer.
func planStatementSQL(report plan.Report, i int) string {
	if i < 0 || i >= len(report.Statements) {
		return ""
	}
	return report.Statements[i].SQL
}

// isCreateAdmissionRefusal reports whether err is one of the create path's
// static admission refusals: decided from the desired statements' shapes
// before anything executes, so it maps to a refusal verdict, not an
// operational error.
func isCreateAdmissionRefusal(err error) bool {
	return errors.Is(err, executor.ErrPartitionOfUnsupported) ||
		errors.Is(err, executor.ErrIfNotExistsUnsupported) ||
		errors.Is(err, executor.ErrUnsupportedCreateStep) ||
		errors.Is(err, executor.ErrDuplicateCreateName)
}

// admitPlan is the all-or-nothing plan-time admission: it refuses the whole
// plan — before anything runs — when the plan cannot converge the table as
// a unit. The checks run from the caller's contract outward: the pinned
// fingerprint first (the caller's approval is void whatever else holds),
// then the destructive guard, then the routed dispositions. An empty
// (already-converged) plan never reaches admission:
// the caller resolves it first, because it carries no plan identity for
// the pin to verify and nothing would run anyway.
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
		refused.Detail += skippedRestDetail(len(report.Statements) - 1)
		return refused, false
	}
	if report.Disposition != router.DispositionExecute {
		refused.Reason, refused.Detail = planRefusal(req, report)
		return refused, false
	}
	return DesiredResult{}, true
}

// skippedRestDetail names what else a whole-plan destructive refusal
// blocked, so the operator knows the size of what is stopped before
// reading the plan: admission is all-or-nothing, and a desired file
// usually carries several edits at once. Zero skipped statements say
// nothing — there is nothing else in the plan to disclose.
func skippedRestDetail(n int) string {
	switch {
	case n == 1:
		return "; admission is all-or-nothing, so the plan's other statement, even if non-destructive, was not run"
	case n > 1:
		return fmt.Sprintf(
			"; admission is all-or-nothing, so the plan's %d other statements, non-destructive ones included, were not run", n)
	default:
		return ""
	}
}

// planRefusal maps the first non-executable planned statement to the typed
// refusal the aggregate result carries, mirroring how [Run] refuses the
// same dispositions at execution time. A greenfield statement the create
// path refuses by shape carries the create path's own cause in the detail.
func planRefusal(req DesiredRequest, report plan.Report) (verdict.Reason, string) {
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
			if cause := createShapeCause(req, report, i); cause != nil {
				return reason, detail(fmt.Sprintf("is refused by the create path: %v", cause))
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

// createShapeCause returns the create path's typed refusal for planned
// statement i of a greenfield plan, nil when the statement is not one the
// create path refused by shape. The plan report carries no field for the
// cause, and the shape check is pure over the desired schema the plan was
// derived from, so it is recomputed here rather than stored. A desired
// schema the planner just derived a plan from cannot fail the same check
// it already passed; if it does, the statement is reported without a cause
// rather than turning a refusal into an error.
func createShapeCause(req DesiredRequest, report plan.Report, i int) error {
	if report.TableExists == nil || *report.TableExists {
		return nil
	}
	if report.Statements[i].Reason != verdict.ReasonUnsupportedStatement {
		return nil
	}
	refused, err := executor.CreateShapeRefusals(req.Schema, req.Desired)
	if err != nil {
		return nil
	}
	if len(refused) != len(report.Statements) {
		// The plan and the desired schema disagree on statement count;
		// no positional cause is trustworthy.
		return nil
	}
	return refused[i]
}

// committedPrefixDetail states how far convergence got when execution
// stopped at statement i (0-based) of n: the statements before it are
// committed and stay committed. A stop on the first statement says plainly
// that nothing committed before it — it does not claim the table is
// untouched, because a failed statement's own committed steps are
// disclosed by that statement's verdict, not here.
func committedPrefixDetail(i, n int, what string) string {
	if i == 0 {
		return fmt.Sprintf("planned statement 1 of %d %s; nothing was committed before it", n, what)
	}
	return fmt.Sprintf("planned statement %d of %d %s; the %d preceding statements committed and remain in effect",
		i+1, n, what, i)
}
