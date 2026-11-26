// This file is the executor's stable outcome vocabulary: every typed
// failure this package can return maps to exactly one flat kebab-case
// Code, the same treatment pkg/lint gave its findings. Orchestrators and
// report consumers branch on the code — never on error prose, which is
// free to change — and the mapping is derived from the typed errors
// themselves, so a code cannot drift from the error it names.

package executor

import "errors"

// Code is the stable string identity of one executor outcome; automation
// branches on it, never on error text. Codes are part of the report
// contract: existing values never change meaning, new outcomes add new
// codes.
type Code string

// The codes an executor outcome can carry.
const (
	// CodeBudgetLockExceeded: the lock was not granted within
	// lock_timeout; nothing was executed.
	CodeBudgetLockExceeded Code = "budget-lock-exceeded"
	// CodeBudgetStatementExceeded: the statement ran past
	// statement_timeout and was cancelled; the change does real work.
	CodeBudgetStatementExceeded Code = "budget-statement-exceeded"
	// CodeLockHolderWaitExceeded: a transaction held a conflicting lock on
	// the table for the retry policy's whole HolderWait; the lock was never
	// requested and nothing was executed.
	CodeLockHolderWaitExceeded Code = "lock-holder-wait-exceeded"
	// CodeRowSecurityPlanChanged requires a fresh preview and SQL review before retry.
	CodeRowSecurityPlanChanged Code = "row-security-plan-changed"
	// CodeRowSecurityRefused requires a declaration, target, or privilege change.
	CodeRowSecurityRefused Code = "row-security-refused"
	// CodeRowSecurityOutcomeUnknown requires catalog inspection before retry.
	CodeRowSecurityOutcomeUnknown Code = "row-security-outcome-unknown"
	// CodeBlockingOutcomeUnknown requires catalog inspection before retry.
	CodeBlockingOutcomeUnknown Code = "blocking-outcome-unknown"
	// CodeCommitOutcomeUnknown requires catalog inspection before retry: a
	// bounded attempt's COMMIT did not return success and may have landed.
	CodeCommitOutcomeUnknown Code = "commit-outcome-unknown"
	// CodeDropOutcomeUnknown requires catalog inspection before retry: a
	// reviewed index drop completed, but the check of which index it
	// dropped failed.
	CodeDropOutcomeUnknown Code = "drop-outcome-unknown"
	// CodeInvalidBlockingBudget identifies an unrepresentable or disabled bound.
	CodeInvalidBlockingBudget Code = "invalid-blocking-budget"
	// CodeUnsupportedAcceptedBlocking identifies a statement outside this narrow path.
	CodeUnsupportedAcceptedBlocking Code = "unsupported-accepted-blocking"
	// CodeCancelledByCaller: the build's statement was cancelled because
	// the caller's own context ended while it ran.
	CodeCancelledByCaller Code = "cancelled-by-caller"
	// CodeCancelledExternally: the build's statement was cancelled from
	// outside the executor — not by its caller, not by its budget.
	CodeCancelledExternally Code = "cancelled-externally"
	// CodeInvalidIndexOwnLeftover: the failed build's own invalid index
	// remains and is proven this run's leftover; the recovery runbook
	// applies. A build that failed on its rows reports CodeDataFault
	// instead, with the leftover still on its Cleanup.
	CodeInvalidIndexOwnLeftover Code = "invalid-index-own-leftover"
	// CodeInvalidIndexBuildInFlight: the invalid index under the requested
	// name is another backend's concurrent build still in progress; wait.
	CodeInvalidIndexBuildInFlight Code = "invalid-index-build-in-flight"
	// CodeInvalidIndexAbandoned: an abandoned invalid index under the
	// requested name sits on the target table with no builder;
	// RebuildAbandonedIndex recovers it.
	CodeInvalidIndexAbandoned Code = "invalid-index-abandoned"
	// CodeInvalidIndexOtherTable: an invalid index under the requested name
	// sits on a different table in the target schema; this change refuses
	// and does not remove it.
	CodeInvalidIndexOtherTable Code = "invalid-index-other-table"
	// CodeInvalidIndexNotDroppable: the invalid index under the requested
	// name sits on the target table but is a partitioned table's index, an
	// index partition, or a constraint's index — not the debris of a
	// failed concurrent build, and not removable by DROP INDEX
	// CONCURRENTLY; this change leaves it in place for an operator.
	CodeInvalidIndexNotDroppable Code = "invalid-index-not-droppable"
	// CodeInvalidIndexBuilderUnobservable: an invalid index under the
	// requested name sits on the target table and this role cannot observe
	// whether a backend is building it; RebuildAbandonedIndex proves the
	// state under the table lock, or a role with pg_read_all_stats can
	// classify it.
	CodeInvalidIndexBuilderUnobservable Code = "invalid-index-builder-unobservable"
	// CodeInvalidIndexUnproven: an invalid index may remain but the
	// catalog state could not be proven; an operator must inspect.
	CodeInvalidIndexUnproven Code = "invalid-index-unproven"
	// CodeNotCreateIndex: the statement handed to the partitioned index
	// build is not a CREATE INDEX.
	CodeNotCreateIndex Code = "not-create-index"
	// CodeNotPartitionedTable: the target of a partitioned index build is
	// not a partitioned table.
	CodeNotPartitionedTable Code = "not-partitioned-table"
	// CodePartitionedIndexNameTaken: a name the partitioned index build
	// needs is held by a relation that is not the requested index; the
	// occupant is left for an operator.
	CodePartitionedIndexNameTaken Code = "partitioned-index-name-taken"
	// CodeForeignTablePartition: the partition tree holds a foreign table,
	// which cannot carry an index; nothing was created.
	CodeForeignTablePartition Code = "foreign-table-partition"
	// CodeUniqueIndexPartitionKey: a unique index does not contain the
	// partition key of every partitioned table in the tree; nothing was
	// created.
	CodeUniqueIndexPartitionKey Code = "unique-index-partition-key"
	// CodeTableLockUnconfirmed: the pool's role cannot see when the session
	// holding the build's table lock started, so the build cannot confirm
	// the lock is its own; nothing was created.
	CodeTableLockUnconfirmed Code = "table-lock-unconfirmed"
	// CodePartitionDetachPending: the partition tree holds a partition
	// whose concurrent detach did not finish, so no index can be attached
	// for it.
	CodePartitionDetachPending Code = "partition-detach-pending"
	// CodeDetachDefaultPartition: a DETACH PARTITION ... CONCURRENTLY on a
	// table that has a DEFAULT partition, which PostgreSQL rejects; nothing
	// was executed.
	CodeDetachDefaultPartition Code = "detach-default-partition"
	// CodeDetachInterrupted: a DETACH PARTITION ... CONCURRENTLY stopped
	// after its first transaction committed, leaving the partition
	// detach-pending; running the same statement again finishes it.
	CodeDetachInterrupted Code = "partition-detach-interrupted"
	// CodePartitionedIndexTablespace: the partitioned index statement has a
	// TABLESPACE clause, which would reach the parent's index and not the
	// partitions' indexes; nothing was created.
	CodePartitionedIndexTablespace Code = "partitioned-index-tablespace"
	// CodePartitionedIndexUnsettled: partitions kept appearing without an
	// index until the build ran out of passes; a rerun resumes.
	CodePartitionedIndexUnsettled Code = "partitioned-index-unsettled"
	// CodePartitionedKeyNameTaken: the name a partitioned key constraint
	// needs on a partition is held by something that is not the requested
	// key; the occupant is left for an operator.
	CodePartitionedKeyNameTaken Code = "partitioned-key-name-taken"
	// CodePartitionedKeyConflict: a partition already has a key constraint
	// that conflicts with the requested one; nothing was added to it.
	CodePartitionedKeyConflict Code = "partitioned-key-conflict"
	// CodePartitionedKeyNullableColumn: a primary key was requested over a
	// column a partition allows to be NULL; nothing was built.
	CodePartitionedKeyNullableColumn Code = "partitioned-key-nullable-column"
	// CodeNotAddKeyConstraint: the statement handed to the partitioned key
	// build is not one ALTER TABLE with a single ADD PRIMARY KEY or ADD
	// UNIQUE.
	CodeNotAddKeyConstraint Code = "not-add-key-constraint"
	// CodePartitionedKeyUnsupportedShape: the partitioned key statement
	// names no constraint or carries a clause the build cannot carry to
	// every partition; nothing was created.
	CodePartitionedKeyUnsupportedShape Code = "partitioned-key-unsupported-shape"
	// CodePartitionedKeyParentConflict: the partitioned table already has a
	// constraint under the requested name that is not the requested key, a
	// relation holds the name its index would take, or a primary key is
	// requested and it has one; nothing was created.
	CodePartitionedKeyParentConflict Code = "partitioned-key-parent-conflict"
	// CodePartitionedKeySubPartitioned: a partition of the target is itself
	// partitioned, which the partitioned key build does not take; nothing
	// was created.
	CodePartitionedKeySubPartitioned Code = "partitioned-key-sub-partitioned"
	// CodePartitionedKeyUnsettled: partitions kept appearing without the key
	// constraint until the build ran out of passes; a rerun resumes.
	CodePartitionedKeyUnsettled Code = "partitioned-key-unsettled"
	// CodeEmptySequence: the sequence had no steps to run.
	CodeEmptySequence Code = "empty-sequence"
	// CodeUnsupportedSequenceStep: a step is not a shape the sequence
	// executor can run safely.
	CodeUnsupportedSequenceStep Code = "unsupported-sequence-step"
	// CodeUnsupportedPartitionedParent identifies partitioned-parent
	// admission refusals.
	CodeUnsupportedPartitionedParent Code = "unsupported-partitioned-parent"
	// CodeNotConcurrentIndexBuild: the statement handed to the concurrent
	// build executor is not a CREATE INDEX CONCURRENTLY.
	CodeNotConcurrentIndexBuild Code = "not-concurrent-index-build"
	// CodeUnnamedIndex: the concurrent build does not name its index, so
	// its outcome could not be verified.
	CodeUnnamedIndex Code = "unnamed-index"
	// CodeUnqualifiedTable: the target table is not schema-qualified at
	// the library boundary.
	CodeUnqualifiedTable Code = "unqualified-table"
	// CodeIfNotExistsUnsupported: CREATE ... IF NOT EXISTS cannot prove
	// what its no-op would mean.
	CodeIfNotExistsUnsupported Code = "if-not-exists-unsupported"
	// CodeCreateCollision: a name the create path needs is already taken
	// on the server; the caller re-diffs the live catalog rather than
	// assuming the occupant's shape.
	CodeCreateCollision Code = "create-collision"
	// CodeCreateNameMismatch: the CREATE TABLE committed but the table does
	// not own a first-choice constraint-index or sequence name the desired
	// file claimed — an occupant took it inside the catalog probe's window
	// and the server chose a suffixed name instead; the table remains for an
	// operator to rename the relation or drop, then re-diff.
	CodeCreateNameMismatch Code = "create-name-mismatch"
	// CodeCreateNamesUnverified: the CREATE TABLE committed but the read of
	// the relation names the table owns did not complete, so whether every
	// first-choice claim was honoured is unknown; the table remains for an
	// operator to compare its names against the desired file, then re-diff.
	// The read's own failure is the cause in the error text, never the code:
	// the code names the state the step left, and that state is a standing
	// table whose names are unproven.
	CodeCreateNamesUnverified Code = "create-names-unverified"
	// CodeCreateOwnerMismatch: the CREATE TABLE committed but the table's
	// catalog owner is not the create owner the proof named; the table
	// remains as committed for an operator to inspect, transfer, or drop —
	// the executor never repairs ownership with ALTER ... OWNER TO.
	CodeCreateOwnerMismatch Code = "create-owner-mismatch"
	// CodeCreateOwnerUnverified: the CREATE TABLE committed but the read of
	// its catalog owner did not complete, so whether it was born under the
	// named owner is unknown; the table remains for an operator to compare
	// its owner against the configured one. As with the unverified names,
	// the read's own failure is the cause in the error text, never the code.
	CodeCreateOwnerUnverified Code = "create-owner-unverified"
	// CodeDuplicateCreateName: the desired set claims the same relation
	// name twice; the conflict is decidable at admission and refused
	// before anything runs.
	CodeDuplicateCreateName Code = "duplicate-create-name"
	// CodePartitionOfUnsupported: CREATE TABLE PARTITION OF locks the
	// partitioned parent, which the absence proof does not cover.
	CodePartitionOfUnsupported Code = "partition-of-unsupported"
	// CodeUnsupportedCreateStep: a desired statement is not a shape the
	// create path can run.
	CodeUnsupportedCreateStep Code = "unsupported-create-step"
	// CodePoolTooSmall: the pool cannot hold every session the operation
	// needs at once — the build session and the verdict connection, or a
	// copy-and-swap run's copy workers and the two connections beside them.
	CodePoolTooSmall Code = "pool-too-small"
	// CodeReplicaIdentityDrop: a native drop would remove what a replica
	// identity index is built on, read again under the table's lock;
	// nothing was dropped. Point REPLICA IDENTITY elsewhere, then re-plan.
	CodeReplicaIdentityDrop Code = "replica-identity-drop"
	// CodeAdoptedIndexDrift: the index a planned ADD CONSTRAINT ... USING
	// INDEX takes over was dropped, replaced, or left invalid after
	// planning, read again under the table's lock; nothing was attached.
	// Re-plan against the index as it stands.
	CodeAdoptedIndexDrift Code = "adopted-index-drift"
	// CodeDroppedIndexDrift: the index a reviewed plan drops is no longer
	// the plain, valid index of the plan's table that the plan named, read
	// again on the session that drops it; nothing was dropped. Re-plan
	// against the catalog as it stands and review the new plan.
	CodeDroppedIndexDrift Code = "dropped-index-drift"
	// CodeDroppedIndexReplaced: a drop that passed its pre-drop read removed
	// an index the plan did not review, because another session gave the
	// name to a different index in the round trip between the read and the
	// drop. Unlike the drift codes, something was dropped: inspect the
	// table's indexes and recreate the dropped one if it should stay.
	CodeDroppedIndexReplaced Code = "dropped-index-replaced"
	// CodeDroppedObjectDrift: the column, constraint, NOT NULL, or table a
	// reviewed plan drops no longer has the definition the plan carried,
	// read again under the table's lock; nothing was dropped. Re-plan
	// against the table as it stands and review the new plan.
	CodeDroppedObjectDrift Code = "dropped-object-drift"
	// CodeDroppedObjectHasDescendants: a reviewed column, constraint, or
	// NOT NULL drop names a table with inheritance children, or a NOT NULL
	// drop (or any drop with ONLY) names a partitioned table with
	// partitions, and the drop would reach their copies too; or a reviewed
	// table drop names a partitioned table or an inheritance parent, which
	// would drop the tables below it. The plan did not review that, so
	// nothing was dropped.
	CodeDroppedObjectHasDescendants Code = "dropped-object-has-descendants"
	// CodeDroppedColumnPartitionKey: a reviewed column drop names a column
	// in the partition key of the partitioned table or of a
	// sub-partitioned partition below it, which PostgreSQL never drops;
	// nothing was dropped.
	CodeDroppedColumnPartitionKey Code = "dropped-column-partition-key"
	// CodeDroppedColumnPartitionDependent: a reviewed column drop on a
	// partitioned table would also remove an object a partition carries of
	// its own on the column (an index, constraint, statistics object, or
	// sequence that is not a copy of the parent's), which the plan did not
	// review; nothing was dropped. Drop or change that object first, then
	// re-plan.
	CodeDroppedColumnPartitionDependent Code = "dropped-column-partition-dependent"
	// CodeDroppedTableHasParent: a reviewed table drop names a partition or
	// an inheritance child, whose rows the table above it also holds;
	// nothing was dropped.
	CodeDroppedTableHasParent Code = "dropped-table-has-parent"
	// CodeDroppedTableHasDependents: the server refused a reviewed table
	// drop because a view, another table's foreign key, or another object
	// depends on the table; nothing was dropped.
	CodeDroppedTableHasDependents Code = "dropped-table-has-dependents"
	// CodeValidatedConstraintDrift: the constraint a planned VALIDATE
	// CONSTRAINT validates was dropped or replaced after planning, read
	// again under the table's lock; nothing was validated. Re-plan against
	// the constraint as it stands.
	CodeValidatedConstraintDrift Code = "validated-constraint-drift"
	// CodeRenamedIndexDrift: the relation an ALTER INDEX ... RENAME TO
	// names is no longer a plain index on the preflighted table, read
	// again under the table's lock — gone, not an index, on another table,
	// or now backing a constraint; nothing was renamed. Re-plan against the
	// catalog as it stands.
	CodeRenamedIndexDrift Code = "renamed-index-drift"
	// CodeEnumTypeDrift: an ALTER TYPE ... ADD VALUE reached a type other
	// than the enum type the engine checked, because another session gave
	// the name to a different type; nothing was added. Plan again against
	// the catalog as it stands.
	CodeEnumTypeDrift Code = "enum-type-drift"
	// CodeEnumLabelExists: the enum type the engine checked already
	// carries the label an ALTER TYPE ... ADD VALUE without IF NOT EXISTS
	// adds, because another change added it after planning; nothing was
	// added. Plan again against the catalog as it stands.
	CodeEnumLabelExists Code = "enum-label-exists"
	// CodeEnumNeighborMissing: the enum type the engine checked no longer
	// carries the label an ALTER TYPE ... ADD VALUE places its value
	// BEFORE or AFTER, because another change renamed or removed it after
	// planning; nothing was added. Plan again against the catalog as it
	// stands.
	CodeEnumNeighborMissing Code = "enum-neighbor-missing"
	// CodeTableNotFound: the statement's qualified table does not exist.
	CodeTableNotFound Code = "table-not-found"
	// CodeDataFault: the server rejected the table's rows as the statement
	// would shape them (SQLSTATE class 22, a value the new type or an index
	// expression will not take; class 23, a constraint the rows break, such
	// as duplicate values under a unique index). The code names the fault,
	// not what was left behind: any rerun meets the same rows until the data
	// or the statement is corrected. A copy-and-swap run tries to abandon
	// itself — shadow, hold on the source, and checkpoint row discarded —
	// and returns *schemachange.AbandonedError only when that finished,
	// which the verdict reports as Abandoned; when the abandon did not
	// finish, start it over with the restart acknowledgement, as the
	// verdict's detail says. A concurrent index build returns an
	// *InvalidIndexError whose Recoverable is false and whose Cleanup
	// still says what it left: its own invalid index occupies the name,
	// so correct the rows, drop the leftover concurrently, and run the
	// change again.
	CodeDataFault Code = "data-fault"
	// CodeInvariantViolation: a breach of the invariant registry; never a
	// retry candidate.
	CodeInvariantViolation Code = "invariant-violation"
	// CodeExecutionFailed: the fallback for a failure outside the typed
	// set — a server error surfaced as-is, a connection failure, a
	// context cancellation. Consumers treat it as an operational error to
	// investigate, not a refusal to branch on.
	CodeExecutionFailed Code = "execution-failed"
)

// Codes returns the closed set of outcome codes. It is part of the report
// contract: adapters enumerate it to know every outcome they must render,
// and the docs test pins every code into the execution-model page so the
// documented vocabulary cannot drift from this one.
func Codes() []Code {
	return []Code{
		CodeBudgetLockExceeded,
		CodeBudgetStatementExceeded,
		CodeLockHolderWaitExceeded,
		CodeBlockingOutcomeUnknown,
		CodeCommitOutcomeUnknown,
		CodeDropOutcomeUnknown,
		CodeRowSecurityPlanChanged,
		CodeRowSecurityRefused,
		CodeRowSecurityOutcomeUnknown,
		CodeInvalidBlockingBudget,
		CodeUnsupportedAcceptedBlocking,
		CodeCancelledByCaller,
		CodeCancelledExternally,
		CodeInvalidIndexOwnLeftover,
		CodeInvalidIndexBuildInFlight,
		CodeInvalidIndexAbandoned,
		CodeInvalidIndexOtherTable,
		CodeInvalidIndexNotDroppable,
		CodeInvalidIndexBuilderUnobservable,
		CodeInvalidIndexUnproven,
		CodeNotCreateIndex,
		CodeNotPartitionedTable,
		CodePartitionedIndexNameTaken,
		CodeForeignTablePartition,
		CodeUniqueIndexPartitionKey,
		CodeTableLockUnconfirmed,
		CodePartitionDetachPending,
		CodeDetachDefaultPartition,
		CodeDetachInterrupted,
		CodePartitionedIndexTablespace,
		CodePartitionedIndexUnsettled,
		CodePartitionedKeyNameTaken,
		CodePartitionedKeyConflict,
		CodePartitionedKeyNullableColumn,
		CodeNotAddKeyConstraint,
		CodePartitionedKeyUnsupportedShape,
		CodePartitionedKeyParentConflict,
		CodePartitionedKeySubPartitioned,
		CodePartitionedKeyUnsettled,
		CodeEmptySequence,
		CodeUnsupportedSequenceStep,
		CodeUnsupportedPartitionedParent,
		CodeNotConcurrentIndexBuild,
		CodeUnnamedIndex,
		CodeUnqualifiedTable,
		CodeIfNotExistsUnsupported,
		CodeCreateCollision,
		CodeCreateNameMismatch,
		CodeCreateNamesUnverified,
		CodeCreateOwnerMismatch,
		CodeCreateOwnerUnverified,
		CodeDuplicateCreateName,
		CodePartitionOfUnsupported,
		CodeUnsupportedCreateStep,
		CodePoolTooSmall,
		CodeReplicaIdentityDrop,
		CodeAdoptedIndexDrift,
		CodeDroppedIndexDrift,
		CodeDroppedIndexReplaced,
		CodeDroppedObjectDrift,
		CodeDroppedObjectHasDescendants,
		CodeDroppedTableHasParent,
		CodeDroppedTableHasDependents,
		CodeDroppedColumnPartitionKey,
		CodeDroppedColumnPartitionDependent,
		CodeValidatedConstraintDrift,
		CodeRenamedIndexDrift,
		CodeEnumTypeDrift,
		CodeEnumLabelExists,
		CodeEnumNeighborMissing,
		CodeTableNotFound,
		CodeDataFault,
		CodeInvariantViolation,
		CodeExecutionFailed,
	}
}

// Permanent reports whether the outcome is decided by the statement, the
// caller's configuration, or the standing catalog: retrying the same call
// unchanged reproduces it, and no executor entry point changes it — an
// author or operator has to act first. It is the floor an adapter's retry
// policy stands on, not its ceiling: a code that is not permanent may still
// be one a particular adapter declines to retry (a statement budget it
// sized as a lease, for one), but a permanent code retried unchanged loops
// for ever. The invalid-index family splits on exactly this line: an entry
// RebuildAbandonedIndex can prove abandoned, a build to wait out, or a
// proof to re-take is not permanent; an entry on another table or one the
// server will not drop concurrently is. The empty code (no error) is not
// permanent.
func (c Code) Permanent() bool {
	switch c {
	case CodeRowSecurityPlanChanged, CodeRowSecurityRefused, CodeInvalidIndexOtherTable,
		CodeInvalidIndexNotDroppable,
		CodeInvalidBlockingBudget,
		CodeUnsupportedAcceptedBlocking,
		CodeNotCreateIndex,
		CodeNotPartitionedTable,
		CodePartitionedIndexNameTaken,
		CodeForeignTablePartition,
		CodeUniqueIndexPartitionKey,
		CodeTableLockUnconfirmed,
		CodePartitionDetachPending,
		CodeDetachDefaultPartition,
		CodePartitionedIndexTablespace,
		CodePartitionedKeyNameTaken,
		CodePartitionedKeyConflict,
		CodePartitionedKeyNullableColumn,
		CodeNotAddKeyConstraint,
		CodePartitionedKeyUnsupportedShape,
		CodePartitionedKeyParentConflict,
		CodePartitionedKeySubPartitioned,
		CodeEmptySequence,
		CodeUnsupportedSequenceStep,
		CodeUnsupportedPartitionedParent,
		CodeNotConcurrentIndexBuild,
		CodeUnnamedIndex,
		CodeUnqualifiedTable,
		CodeIfNotExistsUnsupported,
		CodeCreateCollision,
		CodeCreateNameMismatch,
		CodeCreateOwnerMismatch,
		CodeDuplicateCreateName,
		CodePartitionOfUnsupported,
		CodeUnsupportedCreateStep,
		CodePoolTooSmall,
		CodeReplicaIdentityDrop,
		CodeAdoptedIndexDrift,
		CodeDroppedIndexDrift,
		CodeDroppedIndexReplaced,
		CodeDroppedObjectDrift,
		CodeDroppedObjectHasDescendants,
		CodeDroppedTableHasParent,
		CodeDroppedTableHasDependents,
		CodeDroppedColumnPartitionKey,
		CodeDroppedColumnPartitionDependent,
		CodeValidatedConstraintDrift,
		CodeRenamedIndexDrift,
		CodeEnumTypeDrift,
		CodeEnumLabelExists,
		CodeEnumNeighborMissing,
		CodeTableNotFound,
		CodeDataFault,
		CodeInvariantViolation:
		return true
	}
	return false
}

// OutcomeCode maps an error returned by this package to its stable code.
// A nil error has no outcome code and maps to the empty Code. A
// *SequenceStepError carries its failed step's own cause, so it maps to
// that underlying code — the step position and committed prefix ride on
// the struct itself, not the vocabulary. An error outside the typed set
// maps to CodeExecutionFailed.
//
// An error that carries ErrInvariantViolation maps to
// CodeInvariantViolation whichever typed failure wraps it, so the
// fail-closed code outranks every other one. Each typed error's own Code
// method applies the same rule, so the two never disagree.
func OutcomeCode(err error) Code {
	if err == nil {
		return ""
	}
	// An invariant breach is the fail-closed outcome whichever typed failure
	// carries it, so it outranks every typed code below.
	if errors.Is(err, ErrInvariantViolation) {
		return CodeInvariantViolation
	}
	if stepErr, ok := errors.AsType[*SequenceStepError](err); ok {
		return OutcomeCode(stepErr.Err)
	}
	if invalidErr, ok := errors.AsType[*InvalidIndexError](err); ok {
		return invalidErr.Code()
	}
	if budgetErr, ok := errors.AsType[*BudgetError](err); ok {
		return budgetErr.Code()
	}
	if holderErr, ok := errors.AsType[*LockHolderWaitError](err); ok {
		return holderErr.Code()
	}
	if _, ok := errors.AsType[*RowSecurityOutcomeUnknownError](err); ok {
		return CodeRowSecurityOutcomeUnknown
	}
	if _, ok := errors.AsType[*BlockingOutcomeUnknownError](err); ok {
		return CodeBlockingOutcomeUnknown
	}
	if _, ok := errors.AsType[*CommitOutcomeUnknownError](err); ok {
		return CodeCommitOutcomeUnknown
	}
	if _, ok := errors.AsType[*DropOutcomeUnknownError](err); ok {
		return CodeDropOutcomeUnknown
	}
	return sentinelCode(err)
}

// sentinelCode maps the package's sentinel errors to their codes. The
// invariant sentinel is checked first: an invariant breach is the
// fail-closed outcome regardless of which path wrapped it. The unverified
// create sentinels are checked before the causes they wrap, because the
// code names the state a step left, not the fault that kept it from being
// proven.
func sentinelCode(err error) Code {
	switch {
	case errors.Is(err, ErrInvariantViolation):
		return CodeInvariantViolation
	case errors.Is(err, ErrCreateNamesUnverified):
		// The read's cause rides inside the wrap; the code names what the
		// step left — a standing table whose names are unproven.
		return CodeCreateNamesUnverified
	case errors.Is(err, ErrCreateOwnerUnverified):
		// Same shape: a standing table whose owner is unproven.
		return CodeCreateOwnerUnverified
	case errors.Is(err, ErrInvalidBlockingBudget):
		return CodeInvalidBlockingBudget
	case errors.Is(err, ErrUnsupportedAcceptedBlocking):
		return CodeUnsupportedAcceptedBlocking
	case errors.Is(err, ErrCancelledByCaller):
		return CodeCancelledByCaller
	case errors.Is(err, ErrCancelledExternally):
		return CodeCancelledExternally
	case errors.Is(err, ErrNotCreateIndex):
		return CodeNotCreateIndex
	case errors.Is(err, ErrNotPartitionedTable):
		return CodeNotPartitionedTable
	case errors.Is(err, ErrPartitionedIndexNameTaken):
		return CodePartitionedIndexNameTaken
	case errors.Is(err, ErrForeignTablePartition):
		return CodeForeignTablePartition
	case errors.Is(err, ErrUniqueIndexPartitionKey):
		return CodeUniqueIndexPartitionKey
	case errors.Is(err, ErrTableLockUnconfirmed):
		return CodeTableLockUnconfirmed
	case errors.Is(err, ErrPartitionDetachPending):
		return CodePartitionDetachPending
	case errors.Is(err, ErrDetachDefaultPartition):
		return CodeDetachDefaultPartition
	case errors.Is(err, ErrDetachInterrupted):
		return CodeDetachInterrupted
	case errors.Is(err, ErrPartitionedIndexTablespace):
		return CodePartitionedIndexTablespace
	case errors.Is(err, ErrPartitionedIndexUnsettled):
		return CodePartitionedIndexUnsettled
	case errors.Is(err, ErrPartitionedKeyNameTaken):
		return CodePartitionedKeyNameTaken
	case errors.Is(err, ErrPartitionedKeyConflict):
		return CodePartitionedKeyConflict
	case errors.Is(err, ErrPartitionedKeyNullableColumn):
		return CodePartitionedKeyNullableColumn
	case errors.Is(err, ErrNotAddKeyConstraint):
		return CodeNotAddKeyConstraint
	case errors.Is(err, ErrPartitionedKeyUnsupportedShape):
		return CodePartitionedKeyUnsupportedShape
	case errors.Is(err, ErrPartitionedKeyParentConflict):
		return CodePartitionedKeyParentConflict
	case errors.Is(err, ErrPartitionedKeySubPartitioned):
		return CodePartitionedKeySubPartitioned
	case errors.Is(err, ErrPartitionedKeyUnsettled):
		return CodePartitionedKeyUnsettled
	case errors.Is(err, ErrEmptySequence):
		return CodeEmptySequence
	case errors.Is(err, ErrUnsupportedSequenceStep):
		return CodeUnsupportedSequenceStep
	case errors.Is(err, ErrUnsupportedPartitionedParent):
		return CodeUnsupportedPartitionedParent
	case errors.Is(err, ErrNotConcurrentIndexBuild):
		return CodeNotConcurrentIndexBuild
	case errors.Is(err, ErrUnnamedIndex):
		return CodeUnnamedIndex
	case errors.Is(err, ErrUnqualifiedTable):
		return CodeUnqualifiedTable
	case errors.Is(err, ErrIfNotExistsUnsupported):
		return CodeIfNotExistsUnsupported
	case errors.Is(err, ErrCreateCollision):
		return CodeCreateCollision
	case errors.Is(err, ErrCreateNameMismatch):
		return CodeCreateNameMismatch
	case errors.Is(err, ErrCreateOwnerMismatch):
		return CodeCreateOwnerMismatch
	case errors.Is(err, ErrDuplicateCreateName):
		return CodeDuplicateCreateName
	case errors.Is(err, ErrPartitionOfUnsupported):
		return CodePartitionOfUnsupported
	case errors.Is(err, ErrUnsupportedCreateStep):
		return CodeUnsupportedCreateStep
	case errors.Is(err, ErrPoolTooSmall):
		return CodePoolTooSmall
	case errors.Is(err, ErrRowSecurityPlanChanged):
		return CodeRowSecurityPlanChanged
	case errors.Is(err, ErrRowSecurityRefused):
		return CodeRowSecurityRefused
	case errors.Is(err, ErrReplicaIdentityDrop):
		return CodeReplicaIdentityDrop
	case errors.Is(err, ErrAdoptedIndexDrift):
		return CodeAdoptedIndexDrift
	case errors.Is(err, ErrDroppedIndexDrift):
		return CodeDroppedIndexDrift
	case errors.Is(err, ErrDroppedIndexReplaced):
		return CodeDroppedIndexReplaced
	case errors.Is(err, ErrDroppedObjectDrift):
		return CodeDroppedObjectDrift
	case errors.Is(err, ErrDroppedObjectHasDescendants):
		return CodeDroppedObjectHasDescendants
	case errors.Is(err, ErrDroppedTableHasParent):
		return CodeDroppedTableHasParent
	case errors.Is(err, ErrDroppedTableHasDependents):
		return CodeDroppedTableHasDependents
	case errors.Is(err, ErrDroppedColumnPartitionKey):
		return CodeDroppedColumnPartitionKey
	case errors.Is(err, ErrDroppedColumnPartitionDependent):
		return CodeDroppedColumnPartitionDependent
	case errors.Is(err, ErrValidatedConstraintDrift):
		return CodeValidatedConstraintDrift
	case errors.Is(err, ErrRenamedIndexDrift):
		return CodeRenamedIndexDrift
	case errors.Is(err, ErrEnumTypeDrift):
		return CodeEnumTypeDrift
	case errors.Is(err, ErrEnumLabelExists):
		return CodeEnumLabelExists
	case errors.Is(err, ErrEnumNeighborMissing):
		return CodeEnumNeighborMissing
	case errors.Is(err, ErrTableNotFound):
		return CodeTableNotFound
	default:
		return CodeExecutionFailed
	}
}

// Code returns the budget outcome's stable code. A budget error whose
// cause carries an invariant breach reports the breach, as OutcomeCode does.
func (e *BudgetError) Code() Code {
	if errors.Is(e, ErrInvariantViolation) {
		return CodeInvariantViolation
	}
	switch e.Cause {
	case CauseLock:
		return CodeBudgetLockExceeded
	case CauseStatement:
		return CodeBudgetStatementExceeded
	default:
		// A cause outside the closed set is a programming error; the
		// fallback keeps the mapping total without inventing a meaning.
		return CodeExecutionFailed
	}
}

// Code returns the invalid-index outcome's stable code, derived from the
// same state the error's rendering distinguishes: a build that failed on a
// data fault, then the cleanup state — proven own leftover, another
// backend's build in flight, abandoned on the target table, on another
// table, not droppable, builder unobservable, or unproven. The data fault
// outranks the cleanup state because it names what the operator corrects:
// no handling of the leftover gets a rerun past the same rows. An invariant
// breach in either the build or the cleanup outranks both, as it does in
// OutcomeCode. The cleanup sentinel stays in the chain for errors.Is.
func (e *InvalidIndexError) Code() Code {
	switch {
	case errors.Is(e, ErrInvariantViolation):
		return CodeInvariantViolation
	case e.buildDataFault():
		return CodeDataFault
	case errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex):
		return CodeInvalidIndexOwnLeftover
	case errors.Is(e.Cleanup, ErrInvalidIndexBuildInFlight):
		return CodeInvalidIndexBuildInFlight
	case errors.Is(e.Cleanup, ErrAbandonedInvalidIndex):
		return CodeInvalidIndexAbandoned
	case errors.Is(e.Cleanup, ErrInvalidIndexOnOtherTable):
		return CodeInvalidIndexOtherTable
	case errors.Is(e.Cleanup, ErrInvalidIndexNotDroppable):
		return CodeInvalidIndexNotDroppable
	case errors.Is(e.Cleanup, ErrInvalidIndexBuilderUnobservable):
		return CodeInvalidIndexBuilderUnobservable
	default:
		return CodeInvalidIndexUnproven
	}
}

// Code returns the failed step's own cause code; the step position and
// the committed prefix ride on the struct's fields.
func (e *SequenceStepError) Code() Code {
	return OutcomeCode(e.Err)
}
