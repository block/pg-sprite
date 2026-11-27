package migrate

import (
	"errors"
	"fmt"

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

// destructiveChangeRefusal: neither front door infers permission to discard
// live data or structure. Desired-state admission refuses a destructive plan
// whole; the imperative gate refuses a destructive statement unless the
// destructive acknowledgement names its table.
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

// attachedIndexDropRefusal: a DROP INDEX names an index attached under a
// parent index, which PostgreSQL drops only together with that parent, in
// either form. No engine release or flag changes that; the operator drops
// the parent index instead.
func attachedIndexDropRefusal() verdict.Refusal {
	return verdict.ByDesign(verdict.ReasonUnsupportedStatement)
}

// constraintIndexDropRefusal: a DROP INDEX of a partitioned or attached
// index comes down to an index a primary key, unique, or exclusion
// constraint owns, which PostgreSQL removes only with the constraint, in
// either form. No engine release or flag changes that; the operator drops
// the constraint instead.
func constraintIndexDropRefusal() verdict.Refusal {
	return verdict.ByDesign(verdict.ReasonUnsupportedStatement)
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
		{"attached-index-drop", attachedIndexDropRefusal()},
		{"constraint-index-drop", constraintIndexDropRefusal()},
		{"declarative-foreign-key", plan.ForeignKeyChangeRefusal()},
		{"plan-incoherent", planIncoherentRefusal()},
		{"copy-and-swap-not-enabled", copySwapNotEnabledRefusal()},
		{"copy-and-swap-run-stopped", copySwapRunStoppedRefusal()},
		{"row-security-unsupported", rowSecurityUnsupportedRefusal()},
		{"row-security-missing-table", plan.RowSecurityMissingTableRefusal()},
	}
}

// gateRefusal classifies the statement-type gate's refusal of a kind the
// imperative front door does not admit. ok is false for the admitted kinds
// (ALTER TABLE, CREATE INDEX, ALTER INDEX ... RENAME TO, ALTER TYPE ...
// ADD VALUE). concurrent distinguishes the already-safe maintenance forms —
// which pg-sprite need not wrap — from the plain forms it refuses in favor
// of their concurrent idiom.
func gateRefusal(kind statement.Kind, concurrent bool, target statement.IndexTarget) (verdict.Refusal, bool) {
	switch kind {
	case statement.KindAlterTable, statement.KindCreateIndex, statement.KindRenameIndex, statement.KindAddEnumValue:
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
	default:
		// KindOther: a statement the parse boundary does not name. The
		// engine may learn to route it; nobody else is named to run it.
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
	}
}

// admissionSentinels is the closed set of the sequence executor's and the
// partitioned index and foreign key builds' static admission refusals, decided from the
// statement's text before anything runs, and the build's refusal of a unique
// index that misses a partition key, decided from the catalog before the
// build creates anything, that the imperative front door maps
// to a refusal verdict.
// admissionRefusal walks this set for membership before it classifies, so
// the predicate and the classification cannot disagree.
func admissionSentinels() []error {
	return []error{
		executor.ErrUnsupportedSequenceStep,
		executor.ErrUnnamedIndex,
		executor.ErrIfNotExistsUnsupported,
		executor.ErrPartitionedIndexTablespace,
		executor.ErrUniqueIndexPartitionKey,
		executor.ErrNotAddForeignKey,
		executor.ErrPartitionedForeignKeyUnsupportedShape,
		executor.ErrDetachDefaultPartition,
		executor.ErrNotAddKeyConstraint,
		executor.ErrPartitionedKeyUnsupportedShape,
	}
}

// partitionedForeignKeySentinels is the closed set of the partitioned
// foreign key build's catalog refusals that the imperative front door maps to
// a refusal verdict when the build returned them before it gave any
// partition the foreign key (partitionedForeignKeyRefusalVerdict checks
// that): the parent stands in the way, the referenced table is one the build
// does not take, or a partition is itself partitioned. The build reads these
// before it touches a partition, and the tree again on a later pass, so the
// same error after a partition got its foreign key is a failed verdict
// instead.
func partitionedForeignKeySentinels() []error {
	return []error{
		executor.ErrPartitionedForeignKeyParentConflict,
		executor.ErrPartitionedForeignKeyReferencedTable,
		executor.ErrPartitionedForeignKeySubPartitioned,
	}
}

// partitionedForeignKeyRefusal classifies a partitioned foreign key build's
// catalog refusal. A parent that already has another constraint under the
// name, or the same foreign key under another name, is a catalog collision:
// environmental, since the same statement runs once the table changes. A
// referenced table that is partitioned, foreign, a view, or inside the tree,
// and a sub-partitioned partition, are shapes the build does not take yet: a
// capability boundary. ok is false for an error outside
// partitionedForeignKeySentinels().
func partitionedForeignKeyRefusal(err error) (verdict.Refusal, bool) {
	if !isInSentinelSet(err, partitionedForeignKeySentinels()) {
		return verdict.Refusal{}, false
	}
	if errors.Is(err, executor.ErrPartitionedForeignKeyParentConflict) {
		return verdict.Environmental(verdict.ReasonUnsupportedStatement), true
	}
	return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
}

// admissionRefusal classifies an imperative admission refusal: decided from
// the statement's shape, or for a unique index or key on a partitioned table
// from the tree's partition keys, before anything executes, so it maps to a
// refusal verdict, not an operational error. ok is false for an error outside
// admissionSentinels(), and for a *SequenceStepError wrapper — execution
// started, which is never an admission refusal.
func admissionRefusal(err error) (verdict.Refusal, bool) {
	if _, ok := errors.AsType[*executor.SequenceStepError](err); ok {
		return verdict.Refusal{}, false
	}
	if !isInSentinelSet(err, admissionSentinels()) {
		return verdict.Refusal{}, false
	}
	if errors.Is(err, executor.ErrIfNotExistsUnsupported) {
		// The sentinel form of the if-not-exists create-shape cause.
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	}
	if errors.Is(err, executor.ErrUniqueIndexPartitionKey) {
		// PostgreSQL itself rejects the index in every supported version, so
		// no engine release runs it; the statement has to change.
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	}
	if errors.Is(err, executor.ErrDetachDefaultPartition) {
		// PostgreSQL rejects a concurrent detach beside a DEFAULT partition
		// in every supported version; the table or the statement has to
		// change.
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	}
	return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
}

// partitionedKeySentinels is the closed set of the partitioned key build's
// catalog refusals that the imperative front door maps to a refusal verdict
// when the build returned them before it changed anything
// (partitionedKeyBuild checks that): the parent stands in the way, a primary
// key column allows NULL, or a partition is itself partitioned. The build
// reads these before it touches a partition, and on a later pass too, so the
// same error after the build changed a partition is a failed verdict instead.
func partitionedKeySentinels() []error {
	return []error{
		executor.ErrPartitionedKeyParentConflict,
		executor.ErrPartitionedKeyNullableColumn,
		executor.ErrPartitionedKeySubPartitioned,
	}
}

// partitionedKeyRefusal classifies a partitioned key build's catalog
// refusal. A parent that already has a primary key, a constraint under the
// name, or a relation holding the index's name is a catalog collision, and a
// primary key column that allows NULL waits on the operator making it NOT
// NULL: both are environmental, since the same statement runs once the table
// changes. A sub-partitioned partition is a tree the build does not take yet,
// a capability boundary. ok is false for an error outside
// partitionedKeySentinels().
func partitionedKeyRefusal(err error) (verdict.Refusal, bool) {
	if !isInSentinelSet(err, partitionedKeySentinels()) {
		return verdict.Refusal{}, false
	}
	if errors.Is(err, executor.ErrPartitionedKeySubPartitioned) {
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
	}
	return verdict.Environmental(verdict.ReasonUnsupportedStatement), true
}

// desiredShapeSentinels is the closed set of desired-file shapes the
// planner refuses while it builds the desired table on the scratch
// schema: a CREATE TABLE that names a live parent, refused before the
// build runs; one that declares a constraint twice, which the build shows
// the server kept only once; an expression whose printed text never
// settles; and a TABLESPACE on an index the partitioned index build would
// create, which that build does not honor. RunDesired maps them to a
// refusal, since nothing was planned or executed.
func desiredShapeSentinels() []error {
	return []error{
		schemadiff.ErrDesiredPartitionOf,
		schemadiff.ErrDesiredInherits,
		schemadiff.ErrDesiredConstraintMerged,
		schemadiff.ErrExpressionUnsettled,
		executor.ErrPartitionedIndexTablespace,
	}
}

// desiredShapeRefusal classifies a desired-file shape refusal. A constraint
// declared twice is a mistake in the file, so it is by design; a live
// parent is a capability boundary. ok is false for an error outside
// desiredShapeSentinels().
func desiredShapeRefusal(err error) (verdict.Refusal, bool) {
	if !isInSentinelSet(err, desiredShapeSentinels()) {
		return verdict.Refusal{}, false
	}
	if errors.Is(err, schemadiff.ErrDesiredConstraintMerged) {
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	}
	return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
}

// desiredFileSentinels is the closed set of whole-file refusals
// statement.ParseDesiredFile returns for a file the declarative front door
// does not admit, before anything is planned. desiredFileRefusal walks this
// set for membership before it classifies.
func desiredFileSentinels() []error {
	return []error{
		statement.ErrMultipleCreateTables,
		statement.ErrDesiredPartitions,
		statement.ErrDisallowedStatement,
		statement.ErrEnumTypeAlone,
		statement.ErrEnumLabel,
	}
}

// desiredFileRefusal classifies a whole-file refusal from the desired-file
// parser. A statement the file may not hold is classed as the imperative
// gate classes it when the gate refuses that kind too: a data change, a
// provisioning or catalog statement goes to its owner, and an unnamed
// statement is a capability boundary. A statement the gate admits (a
// change such as ALTER TABLE or DROP INDEX) is refused by design here, since
// a desired file declares state and the imperative door runs changes.
// Partitions are a capability boundary, as a desired PARTITION OF is; a
// second unrelated table is refused by design, since the model is one table
// per file. ok is false for an error outside desiredFileSentinels(), and for
// a disallowed statement that does not carry its kind.
func desiredFileRefusal(err error) (verdict.Refusal, bool) {
	if !isInSentinelSet(err, desiredFileSentinels()) {
		return verdict.Refusal{}, false
	}
	switch {
	case errors.Is(err, statement.ErrDesiredPartitions):
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
	case errors.Is(err, statement.ErrMultipleCreateTables),
		errors.Is(err, statement.ErrEnumTypeAlone),
		errors.Is(err, statement.ErrEnumLabel):
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	}
	disallowed, ok := errors.AsType[*statement.DisallowedStatementError](err)
	if !ok {
		return verdict.Refusal{}, false
	}
	return desiredStatementRefusal(disallowed.Kind)
}

// desiredStatementRefusal classifies a statement kind a desired file may not
// hold. ok is false for the two kinds the file admits.
func desiredStatementRefusal(kind statement.Kind) (verdict.Refusal, bool) {
	switch kind {
	case statement.KindCreateTable, statement.KindCreateIndex:
		return verdict.Refusal{}, false
	case statement.KindDataChange, statement.KindProvisioning, statement.KindCatalogWork, statement.KindOther:
		return gateRefusal(kind, false, statement.IndexTargetNone)
	default:
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	}
}

// DesiredFileRefusal reports whether err is a refusal of the desired file
// itself, returned by [statement.ParseDesiredFile] before anything is
// planned, by the planner while it builds the desired table, or by the diff
// for a change it does not derive, and returns the refusal verdict for it. sql is the refused statement when err does
// not name one (the file's CREATE TABLE for a planner refusal); a parse
// refusal that names its statement reports that one instead. ok is false
// for every other error, including a syntax error, which is an input error
// and not a refusal.
func DesiredFileRefusal(err error, sql string) (verdict.Verdict, bool) {
	refusedStatements := desiredRefusedStatements(err)
	err = decisiveDesiredRefusal(err)
	ref, ok := desiredFileRefusal(err)
	if !ok {
		ref, ok = desiredPlanRefusal(err)
	}
	if !ok {
		return verdict.Verdict{}, false
	}
	detail := fmt.Sprintf("%v; nothing was planned or executed", err)
	if disallowed, isStatement := errors.AsType[*statement.DisallowedStatementError](err); isStatement {
		sql = disallowed.SQL
		detail = "a desired file holds one CREATE TABLE, its indexes, and its row security, or one enum type, " +
			"so this statement is not allowed in it; nothing was planned or executed"
	}
	if extra, isExtra := errors.AsType[*statement.EnumTypeFileError](err); isExtra {
		sql = extra.SQL
		detail = "a desired enum type file holds exactly one CREATE TYPE ... AS ENUM, so this statement belongs " +
			"in a file of its own; nothing was planned or executed"
	}
	if second, isTable := errors.AsType[*statement.SecondTableError](err); isTable {
		sql = second.SQL
		detail = "a desired file holds exactly one CREATE TABLE; nothing was planned or executed"
		if second.Partition {
			detail = "a desired file holds exactly one CREATE TABLE, and the declarative model does not plan " +
				"partitions; nothing was planned or executed"
		}
	}
	v := verdict.Verdict{Statement: sql, Detail: detail}.WithRefusal(ref)
	v.RefusedStatements = refusedStatements
	return v, true
}

// desiredRefusedStatements lists every statement a desired file refused, in
// file order, with where each starts, when it refused more than one. A file
// that refused one statement needs no list: the verdict names that statement.
func desiredRefusedStatements(err error) []verdict.RefusedStatement {
	refused, ok := errors.AsType[*statement.RefusedStatementsError](err)
	if !ok || len(refused.Refusals) < 2 {
		return nil
	}
	statements := make([]verdict.RefusedStatement, len(refused.Refusals))
	for i, refusal := range refused.Refusals {
		statements[i] = verdict.RefusedStatement{
			Statement: refusal.Statement,
			Line:      refusal.Line,
			Column:    refusal.Column,
			Detail:    refusal.Err.Error(),
		}
	}
	return statements
}

// decisiveDesiredRefusal returns the one refusal a desired file's verdict
// describes when the file refuses several statements, so the verdict's class,
// statement, and detail all come from the same statement: the first input
// error, which is not a refusal and so keeps the file from taking a verdict at
// all, else the first partition, else the first breach of the one-table or
// one-type rule, else the first statement the file may not hold, else the
// first refusal. Any other error is returned unchanged.
func decisiveDesiredRefusal(err error) error {
	refused, ok := errors.AsType[*statement.RefusedStatementsError](err)
	if !ok || len(refused.Refusals) == 0 {
		return err
	}
	decisive := refused.Refusals[0]
	for _, refusal := range refused.Refusals[1:] {
		if desiredRefusalRank(refusal) < desiredRefusalRank(decisive) {
			decisive = refusal
		}
	}
	return decisive
}

// desiredRefusalRank orders one statement's refusal by how much of the file's
// verdict it decides, lowest first, in the order desiredFileRefusal tests
// the sentinels.
func desiredRefusalRank(refusal statement.Refusal) int {
	if !isDesiredRefusal(refusal) {
		return -1
	}
	switch {
	case errors.Is(refusal, statement.ErrDesiredPartitions):
		return 0
	case errors.Is(refusal, statement.ErrMultipleCreateTables),
		errors.Is(refusal, statement.ErrEnumTypeAlone),
		errors.Is(refusal, statement.ErrEnumLabel):
		return 1
	case errors.Is(refusal, statement.ErrDisallowedStatement):
		return 2
	default:
		return 3
	}
}

// isDesiredRefusal reports whether one statement's refusal classifies as a
// refusal verdict. One that does not, such as a qualified table name, is an
// input error the author must see as one.
func isDesiredRefusal(refusal statement.Refusal) bool {
	if _, ok := desiredFileRefusal(refusal); ok {
		return true
	}
	_, ok := desiredPlanRefusal(refusal)
	return ok
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
// executors, including the creation proofs and create-path admission of a
// table created with its row security. Operational errors are not refusals.
// Privilege causes take priority over the general admission sentinel they
// also wrap. The executor builds the declaration on the same scratch schema
// as RunDesired, so a desired-file shape refusal is classified the same way
// on both paths.
func RowSecurityRefusal(err error) (verdict.Refusal, bool) {
	var privErr *preflight.PrivilegeError
	if errors.Is(err, executor.ErrRowSecurityPrivileges) || errors.As(err, &privErr) ||
		errors.Is(err, preflight.ErrCreateOwnerNotFound) {
		return insufficientPrivilegesRefusal(), true
	}
	if refusal, ok := desiredShapeRefusal(err); ok {
		return refusal, true
	}
	if errors.Is(err, executor.ErrRowSecurityRefused) {
		return rowSecurityUnsupportedRefusal(), true
	}
	if errors.Is(err, executor.ErrCreateCollision) {
		return createCollisionRefusal(), true
	}
	if r, ok := createAdmissionRefusal(err); ok {
		return r, true
	}
	// A table in a schema that does not exist is missing as surely as one
	// absent from a schema that does: the create path cannot run there.
	if errors.Is(err, executor.ErrTableNotFound) || errors.Is(err, preflight.ErrSchemaNotFound) {
		return plan.RowSecurityMissingTableRefusal(), true
	}
	return verdict.Refusal{}, false
}
