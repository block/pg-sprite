package migrate

import (
	"errors"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// desiredPlanRefusal classifies a planning error that refuses a desired
// file before anything executes: a table shape the planner cannot build, or
// a change the diff does not derive. ok is false for every other error.
func desiredPlanRefusal(err error) (verdict.Refusal, bool) {
	if ref, ok := desiredShapeRefusal(err); ok {
		return ref, true
	}
	return unsupportedChangeRefusal(err)
}

// unsupportedChangeRefusal classifies a change the diff refused to derive,
// by the cause it names. ok is false for an error that is not a
// *schemadiff.UnsupportedChangeError, and for a cause outside
// schemadiff.UnsupportedCauses().
func unsupportedChangeRefusal(err error) (verdict.Refusal, bool) {
	refused, ok := errors.AsType[*schemadiff.UnsupportedChangeError](err)
	if !ok {
		return verdict.Refusal{}, false
	}
	return unsupportedCauseRefusal(refused.Cause)
}

// unsupportedCauseRefusal classifies one diff refusal cause. A storage
// parameter or identity sequence difference is a short catalog change the
// owner runs directly (ALTER TABLE ... SET, ALTER COLUMN ... SET) before
// declaring the result in the file, so it has no online-safety problem. A
// persistence or collation change rewrites the table or column, which the
// planner does not construct. The rest are shapes the diff does not derive:
// table identity (partitioning), row security on the table-only path, and
// column forms the plan cannot carry.
func unsupportedCauseRefusal(cause schemadiff.UnsupportedCause) (verdict.Refusal, bool) {
	switch cause {
	case schemadiff.CauseStorageParameters, schemadiff.CauseIdentitySequence:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerDirectOperator), true
	case schemadiff.CausePersistence, schemadiff.CauseCollationChange:
		return rewriteRequiredRefusal(), true
	case schemadiff.CauseRowSecurity,
		schemadiff.CausePartitionKey,
		schemadiff.CausePartitionAttachment,
		schemadiff.CauseSerialDefault,
		schemadiff.CauseAddedColumnCollation,
		schemadiff.CauseIdentity,
		schemadiff.CauseGenerated:
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
	}
	return verdict.Refusal{}, false
}
