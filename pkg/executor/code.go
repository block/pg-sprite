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
	// CodeCancelledExternally: the build's statement was cancelled from
	// outside the executor before its budget elapsed.
	CodeCancelledExternally Code = "cancelled-externally"
	// CodeInvalidIndexOwnLeftover: the failed build's own invalid index
	// remains and is proven this run's leftover; the recovery runbook
	// applies.
	CodeInvalidIndexOwnLeftover Code = "invalid-index-own-leftover"
	// CodeInvalidIndexPreexisting: an invalid index under the requested
	// name predates this run; it may be another actor's build in progress.
	CodeInvalidIndexPreexisting Code = "invalid-index-preexisting"
	// CodeInvalidIndexUnproven: an invalid index may remain but the
	// catalog state could not be proven; an operator must inspect.
	CodeInvalidIndexUnproven Code = "invalid-index-unproven"
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
	// CodeIfNotExistsUnsupported: CREATE INDEX CONCURRENTLY IF NOT EXISTS
	// cannot prove what its no-op would mean.
	CodeIfNotExistsUnsupported Code = "if-not-exists-unsupported"
	// CodePoolTooSmall: the pool cannot hold the build session and the
	// verdict connection at once.
	CodePoolTooSmall Code = "pool-too-small"
	// CodeTableNotFound: the statement's qualified table does not exist.
	CodeTableNotFound Code = "table-not-found"
	// CodeInvariantViolation: a breach of the invariant registry; never a
	// retry candidate.
	CodeInvariantViolation Code = "invariant-violation"
	// CodeExecutionFailed: the fallback for a failure outside the typed
	// set — a server error surfaced as-is, a connection failure, a
	// context cancellation. Consumers treat it as an operational error to
	// investigate, not a refusal to branch on.
	CodeExecutionFailed Code = "execution-failed"
)

// OutcomeCode maps an error returned by this package to its stable code.
// A nil error has no outcome code and maps to the empty Code. A
// *SequenceStepError carries its failed step's own cause, so it maps to
// that underlying code — the step position and committed prefix ride on
// the struct itself, not the vocabulary. An error outside the typed set
// maps to CodeExecutionFailed.
func OutcomeCode(err error) Code {
	if err == nil {
		return ""
	}
	var stepErr *SequenceStepError
	if errors.As(err, &stepErr) {
		return OutcomeCode(stepErr.Err)
	}
	var invalidErr *InvalidIndexError
	if errors.As(err, &invalidErr) {
		return invalidErr.Code()
	}
	var budgetErr *BudgetError
	if errors.As(err, &budgetErr) {
		return budgetErr.Code()
	}
	return sentinelCode(err)
}

// sentinelCode maps the package's sentinel errors to their codes. The
// invariant sentinel is checked first: an invariant breach is the
// fail-closed outcome regardless of which path wrapped it.
func sentinelCode(err error) Code {
	switch {
	case errors.Is(err, ErrInvariantViolation):
		return CodeInvariantViolation
	case errors.Is(err, ErrCancelledExternally):
		return CodeCancelledExternally
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
	case errors.Is(err, ErrPoolTooSmall):
		return CodePoolTooSmall
	case errors.Is(err, ErrTableNotFound):
		return CodeTableNotFound
	default:
		return CodeExecutionFailed
	}
}

// Code returns the budget outcome's stable code.
func (e *BudgetError) Code() Code {
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
// same cleanup state the error's rendering distinguishes: proven own
// leftover, proven preexisting, or unproven.
func (e *InvalidIndexError) Code() Code {
	switch {
	case errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex):
		return CodeInvalidIndexOwnLeftover
	case errors.Is(e.Cleanup, ErrPreexistingInvalidIndex):
		return CodeInvalidIndexPreexisting
	default:
		return CodeInvalidIndexUnproven
	}
}

// Code returns the failed step's own cause code; the step position and
// the committed prefix ride on the struct's fields.
func (e *SequenceStepError) Code() Code {
	return OutcomeCode(e.Err)
}
