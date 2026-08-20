// Package verdict is the engine's structured outcome contract: every migrate
// invocation ends in exactly one verdict — executed natively, refused with
// a typed reason and, where one exists, a safer native idiom, or failed
// during execution with the executor's stable outcome code and a disclosure
// of what committed before the failure. Refusals use a distinct exit code
// from operational errors. This type is the seam a future orchestrator
// adapter maps onto SchemaBot's ExecutionModeBlocked.
package verdict

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ExitCodeRefused is the process exit code for a refusal verdict — distinct
// from 1, which means an operational error (could not connect, bad flag, SQL
// error). Automation branches on the difference.
const ExitCodeRefused = 2

// ErrRefused is the sentinel the CLI returns after printing a refusal
// verdict, so the entry point can map it to ExitCodeRefused.
var ErrRefused = errors.New("refused")

// Outcome is what happened to the submitted change.
type Outcome string

// The outcomes a migrate run can end in.
const (
	// OutcomeExecuted means the change ran and committed natively within
	// its budgets.
	OutcomeExecuted Outcome = "executed-natively"
	// OutcomeRefused means the change was not executed; Reason says why.
	OutcomeRefused Outcome = "refused"
	// OutcomeFailed means execution was attempted and failed: an
	// operational error, not a refusal — the process still exits 1. Code
	// carries the executor's stable outcome code, and for a mid-sequence
	// failure FailedStep and ExecutedSQL disclose the failed step and the
	// committed prefix whose state remains, so automation can distinguish
	// "nothing happened" from "partial state left behind".
	OutcomeFailed Outcome = "failed"
)

// Reason is the typed cause of a refusal. Reasons are flat kebab-case
// tokens — they are what automation switches on; prose belongs in Detail.
type Reason string

// The refusal reasons Phase 1 can emit.
const (
	// ReasonNone is the zero reason carried by an executed verdict.
	ReasonNone Reason = ""
	// ReasonUnsupportedStatement: only ALTER TABLE is supported.
	ReasonUnsupportedStatement Reason = "unsupported-statement"
	// ReasonIndexStatement: index maintenance has a native safe idiom
	// (CONCURRENTLY) and is never attempted here.
	ReasonIndexStatement Reason = "index-statement"
	// ReasonTableTooLarge: the size guard skipped the optimistic attempt.
	ReasonTableTooLarge Reason = "not-native-safe-table-too-large"
	// ReasonInsufficientPrivileges: the connected role lacks the access
	// the change needs; Detail names the exact missing GRANT (see
	// docs/engine-role.md).
	ReasonInsufficientPrivileges Reason = "insufficient-privileges"
	// ReasonUnsupportedPartitionedParent: the routed plan builds an index
	// on a partitioned parent, for which the required partition-aware
	// online sequence is not implemented.
	ReasonUnsupportedPartitionedParent Reason = "unsupported-partitioned-parent"
	// ReasonBudgetExceeded: the optimistic attempt exceeded its lock or
	// statement budget and was cancelled.
	ReasonBudgetExceeded Reason = "not-native-safe-budget-exceeded"
	// ReasonRewriteRequired: the submitted form blocks and must run as a
	// safer native sequence, but the planner could not construct one (a
	// multi-operation statement, or a pattern it cannot build). Running
	// the submitted form would falsify the plan's own reason, so the
	// engine refuses instead.
	ReasonRewriteRequired Reason = "not-native-safe-rewrite-required"
	// ReasonBackendUnavailable: the change routes to an execution strategy
	// this build does not implement (copy-and-swap).
	ReasonBackendUnavailable Reason = "backend-unavailable"
	// ReasonDestructiveChange: the desired-state plan discards live
	// structure — a dropped column, constraint, or index — and
	// desired-state execution runs no destructive statement without an
	// explicit path for it. The imperative front door remains the way to
	// run a reviewed destructive statement deliberately.
	ReasonDestructiveChange Reason = "destructive-change"
	// ReasonPlanFingerprintMismatch: the plan recomputed at execution time
	// does not carry the fingerprint the caller pinned, so the plan a
	// reviewer approved is not the plan that would execute; nothing runs.
	ReasonPlanFingerprintMismatch Reason = "plan-fingerprint-mismatch"
)

// Cause narrows ReasonBudgetExceeded to the budget that was exceeded, so
// automation can branch on which limit fired without parsing prose.
type Cause string

// The budget causes a refusal can carry.
const (
	// CauseNone is the zero cause for verdicts that are not budget refusals.
	CauseNone Cause = ""
	// CauseLockBudget: the lock was not granted within lock_timeout; nothing
	// was executed.
	CauseLockBudget Cause = "lock-budget"
	// CauseStatementBudget: the statement ran past statement_timeout and was
	// cancelled; the change needs a rewrite.
	CauseStatementBudget Cause = "statement-budget"
)

// Verdict is the structured outcome of one migrate invocation.
type Verdict struct {
	// Outcome is what happened.
	Outcome Outcome `json:"outcome"`
	// Reason is the typed refusal cause; empty when executed.
	Reason Reason `json:"reason,omitempty"`
	// Cause narrows a budget refusal to the budget that fired; empty
	// otherwise.
	Cause Cause `json:"cause,omitempty"`
	// Code is the executor's stable outcome code (executor.OutcomeCode)
	// carried by a failed verdict — flat kebab-case, part of the executor's
	// report contract. It stays a plain string here so this contract
	// package does not depend on the executor. Empty unless Outcome is
	// OutcomeFailed.
	Code string `json:"code,omitempty"`
	// FailedStep is the 1-based position of the sequence step that failed,
	// matching the numbering the planner's partial-failure contracts use;
	// zero when the failure was not a mid-sequence one (a single-statement
	// attempt rolls back and commits nothing).
	FailedStep int `json:"failed_step,omitempty"`
	// FailedStepSQL is the failed step's statement — the step the planner's
	// partial-failure contract says a retry resumes from.
	FailedStepSQL string `json:"failed_step_sql,omitempty"`
	// Attempts is how many bounded attempts ran before a lock-budget
	// refusal, so automation can tell an exhausted bounded retry from a
	// single cancelled attempt; zero for every other verdict.
	Attempts int `json:"attempts,omitempty"`
	// Statement is the submitted SQL.
	Statement string `json:"statement"`
	// Table is the target table (schema-qualified when the statement was),
	// when the statement has one.
	Table string `json:"table,omitempty"`
	// Detail is the human explanation: why refused, or what committed.
	Detail string `json:"detail,omitempty"`
	// SaferIdiom is a native alternative to the refused statement, when one
	// exists (e.g. CREATE INDEX CONCURRENTLY, ADD CONSTRAINT ... NOT VALID).
	SaferIdiom string `json:"safer_idiom,omitempty"`
	// ExecutedSQL is the ordered SQL the engine actually ran and committed.
	// On an executed verdict it is the substituted safer native sequence
	// (empty when the submitted form ran as-is — a non-empty value is what
	// tells automation a substitution happened). On a failed verdict it is
	// the committed prefix that remains: empty means nothing committed.
	ExecutedSQL []string `json:"executed_sql,omitempty"`
	// Forced reports that --force overrode the engine's routing: the
	// submitted form ran as-is instead of a safer substitution or a
	// strategy refusal. It is the machine-readable audit record of the
	// override.
	Forced bool `json:"forced,omitempty"`
}

// JSON renders the verdict as a single JSON object.
func (v Verdict) JSON() (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode verdict: %w", err)
	}
	return string(b), nil
}

// String renders the verdict for humans.
func (v Verdict) String() string {
	var b strings.Builder
	switch v.Outcome {
	case OutcomeExecuted:
		b.WriteString("executed natively")
	case OutcomeRefused:
		fmt.Fprintf(&b, "refused (%s)", v.Reason)
	case OutcomeFailed:
		fmt.Fprintf(&b, "failed (%s)", v.Code)
	default:
		fmt.Fprintf(&b, "unknown outcome %q", string(v.Outcome))
	}
	if v.Table != "" {
		fmt.Fprintf(&b, "\n  table:     %s", v.Table)
	}
	fmt.Fprintf(&b, "\n  statement: %s", v.Statement)
	if v.Attempts > 0 {
		fmt.Fprintf(&b, "\n  attempts:  %d", v.Attempts)
	}
	if v.Detail != "" {
		fmt.Fprintf(&b, "\n  detail:    %s", v.Detail)
	}
	if v.SaferIdiom != "" {
		fmt.Fprintf(&b, "\n  safer:     %s", v.SaferIdiom)
	}
	if v.Forced {
		b.WriteString("\n  forced:    the submitted form ran as-is (force acknowledged)")
	}
	if v.FailedStep > 0 {
		fmt.Fprintf(&b, "\n  failed at: step %d: %s", v.FailedStep, v.FailedStepSQL)
	}
	if len(v.ExecutedSQL) > 0 {
		if v.Outcome == OutcomeFailed {
			b.WriteString("\n  committed before the failure (their state remains):")
		} else {
			b.WriteString("\n  executed as:")
		}
		for i, sql := range v.ExecutedSQL {
			fmt.Fprintf(&b, "\n    %d. %s", i+1, sql)
		}
	}
	return b.String()
}
