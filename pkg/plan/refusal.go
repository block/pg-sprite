package plan

import (
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/verdict"
)

// This file is the plan-side half of the refusal classification registry
// (docs/refusal-classes.md): the keys that travel with a planned statement.
// A class is a property of the typed cause where one exists and of the
// refusal site where none does; nothing is derived from the reason string.
// The front-door half — statement kinds, admission sentinels, and the
// imperative sites — lives in pkg/migrate, and its completeness test covers
// both halves against the production closed sets.

// CreateShapeRefusal classifies the create path's shape refusal of a
// statement in a greenfield plan. The closed key set is
// executor.CreateShapeCauses(); ok is false for a cause outside it, which
// callers treat as an invariant violation rather than classify.
func CreateShapeRefusal(cause executor.CreateShapeCause) (verdict.Refusal, bool) {
	switch cause {
	case executor.CreateShapePartitionOf,
		executor.CreateShapeInherits,
		executor.CreateShapeLike,
		executor.CreateShapeOfType,
		executor.CreateShapeUnsupportedKind:
		// Shapes outside the absence proof the engine could mint later.
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
	case executor.CreateShapeIfNotExists,
		executor.CreateShapeDuplicateName,
		executor.CreateShapeConcurrently:
		// Permanent decisions with a deliberate path the detail names.
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	case executor.CreateShapeMultipleOperations:
		// The statement and operation parse boundaries disagree: a state
		// this build should not produce.
		return verdict.InvariantViolation(verdict.ReasonUnsupportedStatement), true
	default:
		return verdict.Refusal{}, false
	}
}

// PartitionRefusal classifies a partitioned-parent refusal by its cause. The
// closed key set is preflight.PartitionRefusalCauses(); ok is false outside
// it. The four causes span three classes, which is why the plan carries the
// cause rather than a bare refused flag.
func PartitionRefusal(cause preflight.PartitionRefusalCause) (verdict.Refusal, bool) {
	switch cause {
	case preflight.PartitionCauseConcurrentIndexBuild, preflight.PartitionCauseBlockingIndexBuild:
		// The partition-aware concurrent index flow is a planned capability;
		// refusing the blocking substitute is the policy half of the same gap.
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedPartitionedParent), true
	case preflight.PartitionCauseIndexAdoption:
		// No supported PostgreSQL version adopts an index as a constraint on
		// a partitioned parent; waiting for an engine release waits for nothing.
		return verdict.ByDesign(verdict.ReasonUnsupportedPartitionedParent), true
	case preflight.PartitionCauseNotValidForeignKey:
		// The same statement runs on a newer server; the unblocking action is
		// a server upgrade, not an engine release.
		return verdict.Environmental(verdict.ReasonUnsupportedPartitionedParent), true
	default:
		return verdict.Refusal{}, false
	}
}

// RouteRefusal classifies a routed statement the planner has no safe path
// for: an admitted ALTER TABLE operation with no route. The key is the site.
func RouteRefusal() verdict.Refusal {
	return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement)
}
