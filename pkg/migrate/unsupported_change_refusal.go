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
// table identity (partitioning), row security on the table-only path,
// column forms the plan cannot carry, and an unnamed check whose next
// generated name the server would truncate.
//
// An enum type the file declares but the schema lacks is created once with
// CREATE TYPE, which locks no table, and a label the file dropped is renamed
// with ALTER TYPE ... RENAME VALUE: both are catalog changes the owner runs
// directly. A file that lists the live labels in another order asks for
// nothing PostgreSQL can do, so the file is corrected (by design). So is a
// file that names an object after the temporary name a replaced key's new
// index is built under: the object is renamed so the key can be replaced
// without a moment unenforced. A file that removes or changes an index a
// foreign key references is corrected too: the key is dropped or changed
// first, or the index is kept. So is a file that gives a new constraint the
// name of an index it renames: the rename lands in one change and the
// constraint in the next. A
// name that holds another kind of type would have to be replaced, which the
// planner does not construct.
func unsupportedCauseRefusal(cause schemadiff.UnsupportedCause) (verdict.Refusal, bool) {
	switch cause {
	case schemadiff.CauseStorageParameters, schemadiff.CauseIdentitySequence,
		schemadiff.CauseEnumTypeMissing, schemadiff.CauseEnumValueRemoved:
		return verdict.NoOnlineSafetyProblem(verdict.ReasonUnsupportedStatement, verdict.OwnerDirectOperator), true
	case schemadiff.CauseEnumValueReordered, schemadiff.CauseReplacementIndexName, schemadiff.CauseReferencedIndex,
		schemadiff.CauseRenamedIndexName:
		return verdict.ByDesign(verdict.ReasonUnsupportedStatement), true
	case schemadiff.CausePersistence, schemadiff.CauseCollationChange:
		return rewriteRequiredRefusal(), true
	case schemadiff.CauseRowSecurity,
		schemadiff.CausePartitionKey,
		schemadiff.CausePartitionAttachment,
		schemadiff.CauseSerialDefault,
		schemadiff.CauseAddedColumnCollation,
		schemadiff.CauseIdentity,
		schemadiff.CauseGenerated,
		schemadiff.CauseGeneratedCheckName,
		schemadiff.CauseEnumTypeKind:
		return verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), true
	}
	return verdict.Refusal{}, false
}
