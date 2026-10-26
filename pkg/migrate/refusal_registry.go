package migrate

import (
	"errors"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
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

// rewriteRequiredRefusal: the submitted form blocks the table and the
// planner cannot construct the safer sequence it needs.
func rewriteRequiredRefusal() verdict.Refusal {
	return verdict.CapabilityBoundary(verdict.ReasonRewriteRequired)
}

// appBreakingRenameRefusal: a column or table rename breaks every running
// application instance that still uses the old name, and no SQL exists
// for pg-sprite to substitute — the safe sequence lives in the
// application. The deliberate path is the guidance's rollout and then
// the submitted statement under --force, so the refusal is by design,
// not a capability the engine will gain.
func appBreakingRenameRefusal() verdict.Refusal {
	return verdict.ByDesign(verdict.ReasonRewriteRequired)
}

// backendUnavailableRefusal: the plan routes to an execution strategy that
// has no carrying path for the operation in this build.
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
	return plan.DestructiveChangeRefusal()
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
		{"app-breaking-rename", appBreakingRenameRefusal()},
		{"backend-unavailable", backendUnavailableRefusal()},
		{"table-too-large", tableTooLargeRefusal()},
		{"insufficient-privileges", insufficientPrivilegesRefusal()},
		{"budget-exceeded", budgetExceededRefusal()},
		{"destructive-change", destructiveChangeRefusal()},
		{"plan-fingerprint-mismatch", fingerprintMismatchRefusal()},
		{"create-collision", createCollisionRefusal()},
		{"declarative-foreign-key", plan.ForeignKeyChangeRefusal()},
		{"plan-incoherent", planIncoherentRefusal()},
		{"row-security-unsupported", rowSecurityUnsupportedRefusal()},
		{"row-security-missing-table", plan.RowSecurityMissingTableRefusal()},
	}
}

// gateRefusal classifies the statement-type gate's refusal of a kind the
// imperative front door does not admit. ok is false for the admitted kinds
// (ALTER TABLE, CREATE INDEX). concurrent distinguishes the already-safe
// maintenance forms — which pg-sprite need not wrap — from the plain forms
// it refuses in favor of their concurrent idiom.
func gateRefusal(kind statement.Kind, concurrent bool, target statement.IndexTarget) (verdict.Refusal, bool) {
	switch kind {
	case statement.KindAlterTable, statement.KindCreateIndex:
		return verdict.Refusal{}, false
	case statement.KindDropIndex, statement.KindReindex:
		// A concurrent form is the safe idiom to run directly only when
		// PostgreSQL accepts it: DROP INDEX CONCURRENTLY names exactly one
		// index and takes no CASCADE, so a wider concurrent drop is refused
		// as the plain one would be rather than pointed at a direct run the
		// server rejects.
		if concurrent && (kind == statement.KindReindex || target == statement.IndexTargetSingleRelation) {
			return verdict.NoOnlineSafetyProblem(verdict.ReasonIndexStatement, verdict.OwnerDirectOperator), true
		}
		// INV: AB-4 — only a statement whose whole scope is one relation is
		// at the eligible site; a cascading drop is not, so the
		// acknowledgement cannot reach it.
		site := verdict.RefusalSiteIndexOther
		if target == statement.IndexTargetSingleRelation {
			site = verdict.RefusalSiteIndexSingleRelation
		}
		return verdict.ByDesign(verdict.ReasonIndexStatement).WithSite(site), true
	case statement.KindCreateTable:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerDeclarativeFrontDoor), true
	case statement.KindDataChange:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerDataChangeRunner), true
	case statement.KindProvisioning:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerProvisioning), true
	case statement.KindCatalogWork:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerDirectOperator), true
	case statement.KindRenameIndex:
		// An index rename is a route the engine means to take: it needs
		// the index's owning table resolved from the catalog and whether
		// the index backs a constraint, which the front door does not read
		// yet.
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
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

// desiredShapeSentinels is the closed set of desired-file shapes the
// planner refuses before it builds anything: a CREATE TABLE that names a
// live parent. RunDesired maps them to a refusal, since nothing was
// planned or executed.
func desiredShapeSentinels() []error {
	return []error{
		schemadiff.ErrDesiredPartitionOf,
		schemadiff.ErrDesiredInherits,
	}
}

// desiredShapeRefusal classifies a desired-file shape refusal. ok is false
// for an error outside desiredShapeSentinels().
func desiredShapeRefusal(err error) (verdict.Refusal, bool) {
	if !isInSentinelSet(err, desiredShapeSentinels()) {
		return verdict.Refusal{}, false
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

func rowSecurityUnsupportedRefusal() verdict.Refusal {
	return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement)
}

// RowSecurityRefusal classifies typed admission failures from the atomic RLS
// executor. Operational errors are not refusals. Privilege causes take priority
// over the general admission sentinel they also wrap.
func RowSecurityRefusal(err error) (verdict.Refusal, bool) {
	if errors.Is(err, executor.ErrRowSecurityPrivileges) {
		return insufficientPrivilegesRefusal(), true
	}
	if errors.Is(err, executor.ErrRowSecurityRefused) {
		return rowSecurityUnsupportedRefusal(), true
	}
	if errors.Is(err, executor.ErrTableNotFound) {
		return plan.RowSecurityMissingTableRefusal(), true
	}
	return verdict.Refusal{}, false
}
