// Package verdict is the engine's structured outcome contract: every migrate
// invocation ends in exactly one verdict — executed natively, or refused with
// a typed reason and, where one exists, a safer native idiom. Refusals use a
// distinct exit code from operational errors. This type is the seam a future
// orchestrator adapter maps onto SchemaBot's ExecutionModeBlocked.
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

// The two outcomes a migrate run can end in.
const (
	// OutcomeExecuted means the change ran and committed natively within
	// its budgets.
	OutcomeExecuted Outcome = "executed-natively"
	// OutcomeRefused means the change was not executed; Reason says why.
	OutcomeRefused Outcome = "refused"
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
	// ReasonBudgetExceeded: the optimistic attempt exceeded its lock or
	// statement budget and was cancelled.
	ReasonBudgetExceeded Reason = "not-native-safe-budget-exceeded"
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
	return b.String()
}
