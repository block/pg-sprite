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
	"slices"
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
	// structure — a dropped column, constraint, index, or NOT NULL — and
	// desired-state execution runs no destructive statement without an
	// explicit path for it. The imperative front door remains the way to
	// run a reviewed destructive statement deliberately.
	ReasonDestructiveChange Reason = "destructive-change"
	// ReasonPlanFingerprintMismatch: the plan recomputed at execution time
	// does not carry the fingerprint the caller pinned, so the plan a
	// reviewer approved is not the plan that would execute; nothing runs.
	ReasonPlanFingerprintMismatch Reason = "plan-fingerprint-mismatch"
	// ReasonCreateCollision: the create plan's target name is already
	// occupied — a relation or standalone type took it after the plan was
	// derived — so the greenfield create cannot run; the caller re-derives
	// the plan against the live catalog rather than assuming the
	// occupant's shape.
	ReasonCreateCollision Reason = "create-collision"
)

// Reasons returns the closed set of non-zero Reason values. It is part of
// the verdict contract: the tokens are what automation switches on, so the
// set changes only deliberately, every token is pinned by test, and every
// token has a row in docs/cli-output-examples.md's refusal-reason table.
func Reasons() []Reason {
	return []Reason{
		ReasonUnsupportedStatement,
		ReasonIndexStatement,
		ReasonTableTooLarge,
		ReasonInsufficientPrivileges,
		ReasonUnsupportedPartitionedParent,
		ReasonBudgetExceeded,
		ReasonRewriteRequired,
		ReasonBackendUnavailable,
		ReasonDestructiveChange,
		ReasonPlanFingerprintMismatch,
		ReasonCreateCollision,
	}
}

// Class identifies the routing category of a refusal.
type Class string

const (
	// ClassCapabilityBoundary means the engine has no implemented safe route.
	ClassCapabilityBoundary Class = "capability-boundary"
	// ClassNoOnlineSafetyProblem means another owner should run the work.
	ClassNoOnlineSafetyProblem Class = "no-online-safety-problem"
	// ClassByDesign means pg-sprite permanently refuses the form.
	ClassByDesign Class = "by-design"
	// ClassEnvironmental means the current run environment blocked the work.
	ClassEnvironmental Class = "environmental"
	// ClassInvariantViolation reports incoherent engine state.
	ClassInvariantViolation Class = "invariant-violation"
)

// Classes returns the closed set of refusal classes.
func Classes() []Class {
	return []Class{ClassCapabilityBoundary, ClassNoOnlineSafetyProblem, ClassByDesign, ClassEnvironmental, ClassInvariantViolation}
}

// ParseClass validates a refusal class token.
func ParseClass(value string) (Class, error) {
	for _, class := range Classes() {
		if value == string(class) {
			return class, nil
		}
	}
	return "", fmt.Errorf("unknown refusal class %q", value)
}

// Owner identifies who owns work that has no online-safety problem.
type Owner string

const (
	// OwnerDataChangeRunner owns DML and backfills.
	OwnerDataChangeRunner Owner = "data-change-runner"
	// OwnerDeclarativeFrontDoor owns desired catalog convergence.
	OwnerDeclarativeFrontDoor Owner = "declarative-front-door"
	// OwnerDirectOperator means the operator runs the work directly.
	OwnerDirectOperator Owner = "direct-operator"
	// OwnerProvisioning owns access-control and replication provisioning.
	OwnerProvisioning Owner = "provisioning"
)

// Owners returns the closed set of refusal owners.
func Owners() []Owner {
	return []Owner{OwnerDataChangeRunner, OwnerDeclarativeFrontDoor, OwnerDirectOperator, OwnerProvisioning}
}

// ParseOwner validates a refusal owner token.
func ParseOwner(value string) (Owner, error) {
	for _, owner := range Owners() {
		if value == string(owner) {
			return owner, nil
		}
	}
	return "", fmt.Errorf("unknown refusal owner %q", value)
}

// Refusal is proof that a class, reason, and owner form a valid refusal:
// the fields are unexported and the only constructor validates them, so a
// refusal that reaches a renderer through WithRefusal has been classified.
// The classification registries in pkg/plan and pkg/migrate mint these; a
// refusal site never names a class on its own.
type Refusal struct {
	class  Class
	reason Reason
	owner  Owner
	cause  Cause
	site   RefusalSite
}

// RefusalSite is a typed refusal-site discriminator used when a reason spans
// statement shapes but has no underlying cause.
type RefusalSite string

const (
	// RefusalSiteIndexSingleRelation is the plain one-relation DROP INDEX,
	// REINDEX INDEX, or REINDEX TABLE gate site.
	RefusalSiteIndexSingleRelation RefusalSite = "index-statement-single-relation"
	// RefusalSiteIndexOther is a multi-relation DROP INDEX or a REINDEX scope
	// that does not identify one relation.
	RefusalSiteIndexOther RefusalSite = "index-statement-other"
)

// NewRefusal validates and constructs a refusal proof. It rejects a reason
// outside Reasons(), a class outside Classes(), an owner outside Owners(),
// and an owner that is absent when the class is no-online-safety-problem or
// present when it is not.
func NewRefusal(class Class, reason Reason, owner Owner) (Refusal, error) {
	if reason == ReasonNone || !slices.Contains(Reasons(), reason) {
		return Refusal{}, fmt.Errorf("unknown refusal reason %q", reason)
	}
	if _, err := ParseClass(string(class)); err != nil {
		return Refusal{}, err
	}
	if owner != "" {
		if _, err := ParseOwner(string(owner)); err != nil {
			return Refusal{}, err
		}
	}
	// INV: RF-7 — owner is present exactly when the class is
	// no-online-safety-problem.
	if class == ClassNoOnlineSafetyProblem && owner == "" {
		return Refusal{}, fmt.Errorf("refusal class %s requires an owner", class)
	}
	if class != ClassNoOnlineSafetyProblem && owner != "" {
		return Refusal{}, fmt.Errorf("refusal class %s carries no owner, got %q", class, owner)
	}
	return Refusal{class: class, reason: reason, owner: owner}, nil
}

// The per-class constructors are what the classification registries use:
// each fixes its class, and only NoOnlineSafetyProblem takes an owner, so
// the owner rule holds by construction and a registry entry has no error
// path. The registries' completeness tests pin that every entry's reason is
// in Reasons().

// CapabilityBoundary classifies a refusal of work the engine may learn to do.
func CapabilityBoundary(reason Reason) Refusal {
	return Refusal{class: ClassCapabilityBoundary, reason: reason}
}

// NoOnlineSafetyProblem classifies a refusal of work that is safe to run
// elsewhere and names who runs it.
func NoOnlineSafetyProblem(reason Reason, owner Owner) Refusal {
	return Refusal{class: ClassNoOnlineSafetyProblem, reason: reason, owner: owner}
}

// ByDesign classifies a refusal of a form pg-sprite will never run.
func ByDesign(reason Reason) Refusal {
	return Refusal{class: ClassByDesign, reason: reason}
}

// Environmental classifies a refusal the run environment caused.
func Environmental(reason Reason) Refusal {
	return Refusal{class: ClassEnvironmental, reason: reason}
}

// InvariantViolation classifies a refusal of a state this build should not
// have produced.
func InvariantViolation(reason Reason) Refusal {
	return Refusal{class: ClassInvariantViolation, reason: reason}
}

// Class returns the refusal's validated class.
func (r Refusal) Class() Class { return r.class }

// Reason returns the refusal's validated reason.
func (r Refusal) Reason() Reason { return r.reason }

// Owner returns the refusal's validated owner; empty unless the class is
// no-online-safety-problem.
func (r Refusal) Owner() Owner { return r.owner }

// Cause returns the typed cause that narrows the refusal, when one exists.
func (r Refusal) Cause() Cause { return r.cause }

// Site returns the typed refusal site that narrows the refusal, when one exists.
func (r Refusal) Site() RefusalSite { return r.site }

// WithCause returns r narrowed by a typed cause.
func (r Refusal) WithCause(cause Cause) Refusal {
	r.cause = cause
	return r
}

// WithSite returns r narrowed by a typed refusal site.
func (r Refusal) WithSite(site RefusalSite) Refusal {
	r.site = site
	return r
}

// IsZero reports whether r was never constructed through NewRefusal.
func (r Refusal) IsZero() bool { return r == Refusal{} }

// WithRefusal returns v as a refused verdict carrying r's reason, class,
// owner, cause, and full in-process proof. It is the one path from a
// classified refusal onto the verdict contract, so a site that forgets to
// classify has no Reason to set.
func (v Verdict) WithRefusal(r Refusal) Verdict {
	// INV: RF-7, RF-8 — the verdict's refusal fields and in-process proof
	// come from the proof, never from the site.
	v.Outcome = OutcomeRefused
	v.Reason = r.reason
	v.Class = r.class
	v.Owner = r.owner
	v.Cause = r.cause
	v.proof = r
	return v
}

// Refusal reconstructs the classified refusal a refused verdict carries, so
// a caller that aggregates verdicts into its own result propagates the
// class and owner through the same one path instead of copying fields. It
// fails on a verdict that is not refused, or whose reason, class, and owner
// do not validate together — a verdict this build cannot have produced.
// An in-process verdict returns its full proof, re-validated the same way
// and checked against the exported refusal fields, so a proof that never
// passed the constructors, or fields rewritten after WithRefusal, cannot
// reach an eligibility decision. A verdict decoded from JSON reconstructs
// class, reason, owner, and cause, but cannot recover its refusal site;
// site-keyed eligibility therefore fails closed after JSON decoding, while
// cause-keyed eligibility is decidable from the JSON fields.
func (v Verdict) Refusal() (Refusal, error) {
	if v.Outcome != OutcomeRefused {
		return Refusal{}, fmt.Errorf("verdict outcome is %q, not %q", v.Outcome, OutcomeRefused)
	}
	// INV: RF-8 — preserve the full in-process proof; decoded verdicts can
	// reconstruct only the refusal fields represented in JSON.
	if !v.proof.IsZero() {
		return v.provenRefusal()
	}
	r, err := NewRefusal(v.Class, v.Reason, v.Owner)
	if err != nil {
		return Refusal{}, err
	}
	if v.Cause != CauseNone {
		r = r.WithCause(v.Cause)
	}
	return r, nil
}

// provenRefusal returns the in-process proof once it validates under RF-7
// and agrees with the verdict's exported refusal fields. Both checks are
// needed: the per-class constructors do not validate their reason, and the
// exported fields are what a consumer reads while the proof is what an
// eligibility decision consumes.
func (v Verdict) provenRefusal() (Refusal, error) {
	// INV: RF-7, RF-8 — a proof reaches a consumer only when it is a valid
	// classified refusal and the verdict still describes it.
	if _, err := NewRefusal(v.proof.class, v.proof.reason, v.proof.owner); err != nil {
		return Refusal{}, fmt.Errorf("verdict refusal proof: %w", err)
	}
	if v.refusalFieldsDivergeFromProof() {
		return Refusal{}, fmt.Errorf(
			"verdict refusal fields class=%q reason=%q owner=%q cause=%q diverge from proof class=%q reason=%q owner=%q cause=%q",
			v.Class, v.Reason, v.Owner, v.Cause,
			v.proof.class, v.proof.reason, v.proof.owner, v.proof.cause)
	}
	return v.proof, nil
}

// refusalFieldsDivergeFromProof reports whether any exported refusal field
// no longer matches the proof WithRefusal stamped it from.
func (v Verdict) refusalFieldsDivergeFromProof() bool {
	return v.Class != v.proof.class ||
		v.Reason != v.proof.reason ||
		v.Owner != v.proof.owner ||
		v.Cause != v.proof.cause
}

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
	// CauseParentBlockingIndexBuild identifies a blocking index build on a
	// partitioned parent.
	CauseParentBlockingIndexBuild Cause = "parent-blocking-index-build"
	// CauseParentConcurrentIndexBuild identifies a concurrent index build on
	// a partitioned parent.
	CauseParentConcurrentIndexBuild Cause = "parent-concurrent-index-build"
	// CauseParentIndexAdoption identifies index adoption on a partitioned parent.
	CauseParentIndexAdoption Cause = "parent-index-adoption"
	// CauseParentNotValidForeignKey identifies a NOT VALID foreign key on a
	// partitioned parent.
	CauseParentNotValidForeignKey Cause = "parent-not-valid-foreign-key"
)

// Verdict is the structured outcome of one migrate invocation.
type Verdict struct {
	// proof is the full in-process refusal WithRefusal stamped the exported
	// fields from. It is deliberately outside the JSON contract: the refusal
	// site it carries is not a wire field, so it does not survive decoding,
	// and a decoded verdict never compares equal to the in-process verdict it
	// was encoded from. Refusal() is the only reader.
	proof Refusal

	// Outcome is what happened.
	Outcome Outcome `json:"outcome"`
	// Reason is the typed refusal cause; empty when executed.
	Reason Reason `json:"reason,omitempty"`
	// Class identifies how a consumer routes a refusal.
	Class Class `json:"class,omitempty"`
	// Owner identifies who owns work with no online-safety problem.
	Owner Owner `json:"owner,omitempty"`
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
	if v.Outcome == OutcomeRefused {
		fmt.Fprintf(&b, "\n  class:     %s", v.Class)
		if v.Owner != "" {
			fmt.Fprintf(&b, "\n  owner:     %s", v.Owner)
		}
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
