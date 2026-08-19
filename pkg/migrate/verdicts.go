package migrate

import (
	"errors"
	"fmt"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Gate is the statement-type gate: ALTER TABLE and CREATE INDEX proceed to
// classification (a blocking CREATE INDEX is substituted with its
// concurrent build, a submitted concurrent build is driven directly); the
// index-maintenance forms the executor cannot drive yet are pointed at
// their concurrent idiom, everything else is unsupported. Refused
// statements are never executed. Gate needs no database, so a caller can
// refuse before dialing; [Run] re-checks it regardless.
func Gate(st statement.Statement) (verdict.Verdict, bool) {
	v := verdict.Verdict{Outcome: verdict.OutcomeRefused, Statement: st.SQL()}
	switch st.Kind() {
	case statement.KindAlterTable, statement.KindCreateIndex:
		return verdict.Verdict{}, false
	case statement.KindDropIndex, statement.KindReindex:
		v.Reason = verdict.ReasonIndexStatement
		v.Detail, v.SaferIdiom = indexAdvice(st)
	case statement.KindCreateTable:
		v.Reason = verdict.ReasonUnsupportedStatement
		v.Detail = "migrate changes an existing table; to converge a table onto a desired-state CREATE TABLE, use the declarative front-end"
		v.SaferIdiom = "pg-sprite diff --desired schema.sql"
	case statement.KindOther:
		v.Reason = verdict.ReasonUnsupportedStatement
		v.Detail = "only ALTER TABLE and CREATE INDEX statements are supported by the imperative front door"
	}
	return v, true
}

// indexAdvice explains an index-statement refusal for the maintenance forms
// the executor does not drive (DROP INDEX, REINDEX). The already-concurrent
// forms carry no safer idiom: suggesting the statement the user submitted
// would confuse a human once and send a resubmitting automation into a loop.
func indexAdvice(st statement.Statement) (detail, saferIdiom string) {
	if st.Concurrent() {
		return "this is already the safe concurrent idiom; pg-sprite does not drive this maintenance form yet — run it directly against the database", ""
	}
	switch st.Kind() {
	case statement.KindDropIndex:
		return "a plain DROP INDEX takes ACCESS EXCLUSIVE on the table; the concurrent drop does not", "DROP INDEX CONCURRENTLY"
	case statement.KindReindex:
		return "a plain REINDEX blocks writes; the concurrent rebuild does not", "REINDEX ... CONCURRENTLY"
	default:
		return "", ""
	}
}

// privilegeVerdict is the refusal for a connected role that lacks the access
// the routed change needs. The error already names the failed catalog check
// and the exact provisioning statement, so it is the detail verbatim. A
// refused forced attempt still records the override: the operator asked for
// the submitted form and the role could not run it.
func privilegeVerdict(st statement.Statement, privErr *preflight.PrivilegeError, forced bool) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonInsufficientPrivileges,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail:    privErr.Error(),
	}
}

// partitionedParentVerdict refuses unsupported execution steps on a
// partitioned parent before the sequence executor runs anything.
func partitionedParentVerdict(st statement.Statement, partitionErr *preflight.UnsupportedPartitionedParentError,
	forced bool) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonUnsupportedPartitionedParent,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail:    partitionErr.Error(),
	}
}

// rewriteRequiredVerdict is the refusal for a statement whose submitted
// form blocks but for which the planner could not construct the safer
// native sequence — a multi-operation statement, or a pattern it cannot
// build. Running the submitted form would falsify the plan's own reason.
func rewriteRequiredVerdict(st statement.Statement) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonRewriteRequired,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: "the submitted form blocks and must run as a safer native sequence, but pg-sprite could not " +
			"construct one for this statement; submit each operation as its own single-operation statement " +
			"so the engine can build its safer form (run with --dry-run to see each operation's classification)",
	}
}

// backendUnavailableVerdict is the refusal for a change that routes to an
// execution strategy this build does not implement.
func backendUnavailableVerdict(st statement.Statement, rs router.Statement) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonBackendUnavailable,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: fmt.Sprintf("the change requires the %s strategy, which this build does not implement yet: "+
			"PostgreSQL would rewrite the table under ACCESS EXCLUSIVE for the whole operation", rs.Backend),
	}
}

// routeRefusalVerdict is the refusal for a statement the planner refused:
// it carries the refused operations by name so the operator knows which
// part of the statement the engine does not know a safe path for.
func routeRefusalVerdict(st statement.Statement, rs router.Statement) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonUnsupportedStatement,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    "the planner knows no safe path for this statement",
	}
	for _, d := range rs.Decisions {
		if d.Route == planner.RouteRefuse {
			v.Detail = fmt.Sprintf("the planner knows no safe path for %s", d.Operation)
			break
		}
	}
	return v
}

// sizeGuardVerdict is the refusal for tables above the size threshold, where
// even a budget-bounded attempt would visibly stall the table. A refused
// forced attempt still records the override: the operator asked for the
// submitted form and the guard said no.
func sizeGuardVerdict(st statement.Statement, sizeErr *preflight.SizeError, forced bool) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonTableTooLarge,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail: fmt.Sprintf("table is %d bytes on disk (heap, indexes, and TOAST), above the %d-byte "+
			"--max-table-size threshold. pg-sprite cannot yet prove this change is instant on a table this "+
			"size; if it requires a rewrite, a cancelled attempt is not a free probe — it would hold "+
			"ACCESS EXCLUSIVE doing rewrite work for the whole budget",
			sizeErr.TotalBytes, sizeErr.LimitBytes),
	}
}

// admissionRefusalVerdict is the refusal for a statement the gate admits but
// the executor's static admission refuses before anything executes: an
// unnamed index build, IF NOT EXISTS on a concurrent build, or a substituted
// step shape the sequence executor does not drive yet (DETACH PARTITION
// CONCURRENTLY). The typed error carries the explanation.
func admissionRefusalVerdict(st statement.Statement, err error, forced bool) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonUnsupportedStatement,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail:    fmt.Sprintf("the engine cannot run this statement safely: %v; nothing was executed", err),
	}
}

// budgetVerdict is the refusal for an attempt that exceeded its lock or
// statement budget and was cancelled without executing. online tailors the
// statement-budget advice: a submitted form the plan proved an online idiom
// (a concurrent build, a lone VALIDATE) needs a larger budget, not a
// different strategy, while a blind attempt that ran past its budget is
// doing rewrite work. A refused forced attempt still records the override.
func budgetVerdict(st statement.Statement, budgetErr *executor.BudgetError, forced, online bool) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonBudgetExceeded,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
	}
	switch budgetErr.Cause {
	case executor.CauseLock:
		v.Cause = verdict.CauseLockBudget
		v.Attempts = budgetErr.Attempts
		if budgetErr.Attempts > 1 {
			v.Detail = fmt.Sprintf("the lock was not granted within the %s lock budget on any of %d bounded "+
				"attempts: the table is too contended for a blind attempt; nothing was executed",
				budgetErr.Budget, budgetErr.Attempts)
			break
		}
		v.Detail = fmt.Sprintf("the lock was not granted within the %s lock budget: the table is too "+
			"contended right now; nothing was executed", budgetErr.Budget)
	case executor.CauseStatement:
		v.Cause = verdict.CauseStatementBudget
		if online {
			v.Detail = fmt.Sprintf("cancelled after the %s budget: the statement already is the safe online "+
				"idiom — the work needs more time, not a different strategy; retry with a larger budget for "+
				"this step class", budgetErr.Budget)
		} else {
			v.Detail = fmt.Sprintf("cancelled after the %s statement budget: the change does real rewrite work, "+
				"not an in-place catalog change, and needs a copy-and-swap rewrite that pg-sprite does not perform yet. "+
				"If it adds a constraint, ADD CONSTRAINT ... NOT VALID followed by VALIDATE CONSTRAINT avoids the long lock",
				budgetErr.Budget)
		}
	default:
		v.Detail = budgetErr.Error()
	}
	return v
}

// failureVerdict maps an operational execution failure to its failed
// verdict: the executor's stable outcome code, and for a mid-sequence
// failure the failed step and the committed prefix whose state remains.
// It is the machine-readable twin of the returned error — automation
// branches on Code and ExecutedSQL instead of parsing stderr prose. A
// single bounded attempt (the submitted form, forced or not) rolls back on
// failure, so it carries no step and an empty committed prefix.
func failureVerdict(st statement.Statement, err error,
	rep executor.SequenceReport, forced bool) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.OutcomeCode(err)),
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail:    "execution failed; nothing committed — a started bounded attempt rolls back",
	}
	var stepErr *executor.SequenceStepError
	if !errors.As(err, &stepErr) {
		return v
	}
	v.FailedStep = stepErr.Step
	v.FailedStepSQL = stepErr.SQL
	for _, s := range rep.Steps {
		v.ExecutedSQL = append(v.ExecutedSQL, s.SQL)
	}
	if len(v.ExecutedSQL) > 0 {
		v.Detail = fmt.Sprintf("sequence step %d of %d failed; the %d committed steps' state remains — the planner sequence's partial-failure contract says how a retry resumes",
			stepErr.Step, stepErr.Total, len(v.ExecutedSQL))
	} else {
		v.Detail = fmt.Sprintf("sequence step %d of %d failed; no earlier steps had committed — Code names the outcome and any state the failed step itself left",
			stepErr.Step, stepErr.Total)
	}
	return v
}

// execRefusal maps an execution failure to its refusal verdict, when the
// failure belongs to the refusal contract rather than the operational-error
// exit: a static admission refusal (decided before anything executed), or a
// budget cancellation of a non-substituted attempt. An *InvalidIndexError
// is never a refusal, even when a budget cancellation is buried inside it —
// invalid-index debris is the one outcome that needs an operator, and its
// typed error carries the recovery guidance a budget verdict would conceal.
func execRefusal(st statement.Statement, err error,
	substituted, forced, online bool) (verdict.Verdict, bool) {
	if err == nil {
		return verdict.Verdict{}, false
	}
	if isAdmissionRefusal(err) {
		return admissionRefusalVerdict(st, err, forced), true
	}
	var invalidErr *executor.InvalidIndexError
	if errors.As(err, &invalidErr) {
		return verdict.Verdict{}, false
	}
	var budgetErr *executor.BudgetError
	if !substituted && errors.As(err, &budgetErr) {
		// The blind attempt of the submitted form exceeded a budget and was
		// cancelled without committing — the Phase 1 refusal contract. A
		// forced attempt is bounded by the same budgets: force overrides
		// routing, never the executor's protections.
		return budgetVerdict(st, budgetErr, forced, online), true
	}
	return verdict.Verdict{}, false
}

// isAdmissionRefusal reports whether err is one of the executor's static
// admission refusals: decided from the statement's shape before anything
// executes, so it maps to a refusal verdict, not an operational error. A
// *SequenceStepError wrapper means execution started, which is never an
// admission refusal.
func isAdmissionRefusal(err error) bool {
	var stepErr *executor.SequenceStepError
	if errors.As(err, &stepErr) {
		return false
	}
	return errors.Is(err, executor.ErrUnsupportedSequenceStep) ||
		errors.Is(err, executor.ErrUnnamedIndex) ||
		errors.Is(err, executor.ErrIfNotExistsUnsupported)
}

// onlineIdiomPlan reports whether the plan proved every operation an online
// idiom (CONCURRENTLY, NOT VALID, VALIDATE): the submitted form already is
// the safe pattern, and running long on a large table is its purpose.
func onlineIdiomPlan(p planner.Plan) bool {
	for _, d := range p.Decisions {
		if d.Reason != planner.ReasonOnlineIdiom {
			return false
		}
	}
	return true
}

// sizeGuardApplies reports whether the size guard protects this run. It
// guards exactly the blind attempt of the submitted form: when the engine
// substituted the planner's safer sequence, or when the plan proved every
// operation an online idiom, long work on a large table is the pattern's
// purpose and the guard would refuse the very tables the pattern serves.
func sizeGuardApplies(p planner.Plan, substituted bool) bool {
	return !substituted && !onlineIdiomPlan(p)
}
