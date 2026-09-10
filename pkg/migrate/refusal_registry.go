package migrate

import (
	"errors"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// This file is the front-door half of the refusal classification registry
// (docs/refusal-classes.md). Every refusal the imperative and desired-state
// front doors mint is classified here, keyed on a typed discriminator where
// one exists — the statement kind, an admission sentinel, a partition cause,
// a create-shape cause — and on the refusal site where none does. The
// plan-side half (plan.CreateShapeRefusal, plan.PartitionRefusal,
// plan.RouteRefusal) is reused rather than restated. Nothing is derived from
// the reason string; a new discriminator value or site is classified by its
// author, and TestRefusalRegistryIsComplete fails until it is.

// The site-keyed refusals: no typed discriminator lies beneath their reason,
// so each site's classification is a total function and siteRefusals()
// enumerates them for the registry's completeness test.

// rewriteRequiredRefusal: the planner cannot construct the safer sequence
// the submitted form needs.
func rewriteRequiredRefusal() verdict.Refusal {
	return verdict.CapabilityBoundary(verdict.ReasonRewriteRequired)
}

// backendUnavailableRefusal: the plan routes to an execution strategy this
// build lacks.
func backendUnavailableRefusal() verdict.Refusal {
	return verdict.CapabilityBoundary(verdict.ReasonBackendUnavailable)
}

// tableTooLargeRefusal: the size policy prevents a blind bounded attempt.
func tableTooLargeRefusal() verdict.Refusal {
	return verdict.Environmental(verdict.ReasonTableTooLarge)
}

// insufficientPrivilegesRefusal: the connected role lacks a required grant.
func insufficientPrivilegesRefusal() verdict.Refusal {
	return verdict.Environmental(verdict.ReasonInsufficientPrivileges)
}

// budgetExceededRefusal: the lock or statement budget cancelled an attempt.
func budgetExceededRefusal() verdict.Refusal {
	return verdict.Environmental(verdict.ReasonBudgetExceeded)
}

// destructiveChangeRefusal: desired-state execution never infers permission
// to discard live structure; the deliberate path is the imperative statement.
func destructiveChangeRefusal() verdict.Refusal {
	return verdict.ByDesign(verdict.ReasonDestructiveChange)
}

// fingerprintMismatchRefusal: the live table or desired schema moved under
// the reviewed plan.
func fingerprintMismatchRefusal() verdict.Refusal {
	return verdict.Environmental(verdict.ReasonPlanFingerprintMismatch)
}

// createCollisionRefusal: a target or claimed name is occupied at apply time.
func createCollisionRefusal() verdict.Refusal {
	return verdict.Environmental(verdict.ReasonCreateCollision)
}

// planIncoherentRefusal: a planned statement whose disposition this build
// does not know, a refused planned statement carrying no class, or a plan
// whose aggregate disposition no statement carries — a report this build
// cannot have produced.
func planIncoherentRefusal() verdict.Refusal {
	return verdict.InvariantViolation(verdict.ReasonUnsupportedStatement)
}

// siteRefusal names one site-keyed refusal for the completeness test.
type siteRefusal struct {
	site    string
	refusal verdict.Refusal
}

// siteRefusals is the closed walk of the site-keyed refusals.
func siteRefusals() []siteRefusal {
	return []siteRefusal{
		{"rewrite-required", rewriteRequiredRefusal()},
		{"backend-unavailable", backendUnavailableRefusal()},
		{"table-too-large", tableTooLargeRefusal()},
		{"insufficient-privileges", insufficientPrivilegesRefusal()},
		{"budget-exceeded", budgetExceededRefusal()},
		{"destructive-change", destructiveChangeRefusal()},
		{"plan-fingerprint-mismatch", fingerprintMismatchRefusal()},
		{"create-collision", createCollisionRefusal()},
		{"plan-incoherent", planIncoherentRefusal()},
	}
}

// gateRefusal classifies the statement-type gate's refusal of a kind the
// imperative front door does not admit. ok is false for the admitted kinds
// (ALTER TABLE, CREATE INDEX). concurrent distinguishes the already-safe
// maintenance forms — which pg-sprite need not wrap — from the plain forms
// it refuses in favor of their concurrent idiom.
func gateRefusal(kind statement.Kind, concurrent bool) (verdict.Refusal, bool) {
	switch kind {
	case statement.KindAlterTable, statement.KindCreateIndex:
		return verdict.Refusal{}, false
	case statement.KindDropIndex, statement.KindReindex:
		if concurrent {
			return verdict.NoOnlineSafetyProblem(verdict.ReasonIndexStatement, verdict.OwnerDirectOperator), true
		}
		return verdict.ByDesign(verdict.ReasonIndexStatement), true
	case statement.KindCreateTable:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerDeclarativeFrontDoor), true
	case statement.KindDataChange:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerDataChangeRunner), true
	case statement.KindProvisioning:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerProvisioning), true
	case statement.KindCatalogWork:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerDirectOperator), true
	default:
		// KindOther: a statement the parse boundary does not name. The
		// engine may learn to route it; nobody else is named to run it.
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
	}
}

// admissionSentinels is the closed set of the sequence executor's static
// admission refusals the imperative front door maps to a refusal verdict.
// admissionRefusal walks this set for membership before it classifies, so
// the predicate and the classification cannot disagree.
func admissionSentinels() []error {
	return []error{
		executor.ErrUnsupportedSequenceStep,
		executor.ErrUnnamedIndex,
		executor.ErrIfNotExistsUnsupported,
	}
}

// admissionRefusal classifies an imperative admission refusal: decided from
// the statement's shape before anything executes, so it maps to a refusal
// verdict, not an operational error. ok is false for an error outside
// admissionSentinels(), and for a *SequenceStepError wrapper — execution
// started, which is never an admission refusal.
func admissionRefusal(err error) (verdict.Refusal, bool) {
	var stepErr *executor.SequenceStepError
	if errors.As(err, &stepErr) {
		return verdict.Refusal{}, false
	}
	if !isInSentinelSet(err, admissionSentinels()) {
		return verdict.Refusal{}, false
	}
	if errors.Is(err, executor.ErrIfNotExistsUnsupported) {
		// The sentinel form of the if-not-exists create-shape cause.
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	}
	return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
}

// createAdmissionSentinels is the closed set of the create path's static
// admission refusals the desired-state front door maps to a refusal.
// createAdmissionRefusal walks this set for membership before it classifies.
func createAdmissionSentinels() []error {
	return []error{
		executor.ErrPartitionOfUnsupported,
		executor.ErrIfNotExistsUnsupported,
		executor.ErrUnsupportedCreateStep,
		executor.ErrDuplicateCreateName,
	}
}

// createAdmissionRefusal classifies a create-path admission refusal. Three
// of the sentinels are the error form of a create-shape cause and carry the
// cause's class (TestRefusalRegistryIsComplete pins the two spellings
// together). ok is false for an error outside createAdmissionSentinels().
func createAdmissionRefusal(err error) (verdict.Refusal, bool) {
	if !isInSentinelSet(err, createAdmissionSentinels()) {
		return verdict.Refusal{}, false
	}
	if errors.Is(err, executor.ErrIfNotExistsUnsupported) || errors.Is(err, executor.ErrDuplicateCreateName) {
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	}
	return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
}

// createShapeSentinelCauses pairs each create-admission sentinel that is the
// error form of a create-shape cause with that cause, so the registry test
// can pin that both spellings classify identically.
func createShapeSentinelCauses() map[error]executor.CreateShapeCause {
	return map[error]executor.CreateShapeCause{
		executor.ErrIfNotExistsUnsupported: executor.CreateShapeIfNotExists,
		executor.ErrDuplicateCreateName:    executor.CreateShapeDuplicateName,
		executor.ErrPartitionOfUnsupported: executor.CreateShapePartitionOf,
	}
}

// partitionRefusal classifies the imperative front door's partitioned-parent
// refusal by its cause; the plan-side registry owns the mapping.
func partitionRefusal(cause preflight.PartitionRefusalCause) (verdict.Refusal, bool) {
	return plan.PartitionRefusal(cause)
}

// isInSentinelSet reports whether err matches any sentinel in set.
func isInSentinelSet(err error, set []error) bool {
	for _, sentinel := range set {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
