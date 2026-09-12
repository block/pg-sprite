package executor

import (
	"errors"
)

// Typed admission refusals for the concurrent index build. The executor
// re-verifies the statement shape itself — a caller's classification is a
// request, not a proof (see SAFETY.md).
var (
	// ErrNotConcurrentIndexBuild is returned when the statement is not a
	// single CREATE INDEX ... CONCURRENTLY.
	ErrNotConcurrentIndexBuild = errors.New("statement is not a CREATE INDEX CONCURRENTLY")
	// ErrUnnamedIndex is returned when the concurrent build does not name
	// its index: without a deterministic identity there is no idempotent
	// way to find and recover the invalid leftover of a failed build.
	ErrUnnamedIndex = errors.New("concurrent index build must name its index")
	// ErrUnqualifiedTable is returned when the target table is not
	// schema-qualified. The post-failure catalog verdict re-resolves the
	// table on another session, where search_path cannot be proven
	// identical to the build session's — an unqualified name could resolve
	// to a different table and turn the verdict into a false clean.
	ErrUnqualifiedTable = errors.New("concurrent index build must schema-qualify its table")
	// ErrIfNotExistsUnsupported is returned for any CREATE ... IF NOT
	// EXISTS. The clause checks only the name: it succeeds as a no-op
	// while an unrelated relation — or, for a concurrent build, an
	// invalid index — owns that name, so an executor could report
	// success over a relation it cannot vouch for.
	ErrIfNotExistsUnsupported = errors.New(CreateShapeIfNotExists.Description())
	// ErrInvalidIndexBuildInFlight is returned (inside an *InvalidIndexError
	// carrying the builder's PID) when the invalid index under the
	// requested name is another backend's concurrent build still in
	// progress: pg_stat_progress_create_index reports a live command on
	// that exact index OID. A concurrent build is invalid until it
	// finishes, so the entry may be healthy and hours in; nothing here may
	// touch it, and the build refuses because the name is occupied. The
	// answer is to wait for the builder to finish or fail (see
	// docs/invalid-index-recovery.md).
	ErrInvalidIndexBuildInFlight = errors.New("an invalid index with this name is another backend's concurrent build still in progress")
	// ErrAbandonedInvalidIndex is returned (inside an *InvalidIndexError)
	// when an invalid index under the requested name sits on the target
	// table with no backend building it: the debris of an earlier failed
	// build, this executor's or anyone's. It cannot become valid on its
	// own, so the build refuses — the name is occupied — but the state is
	// recoverable: RebuildAbandonedIndex proves the abandonment under lock,
	// removes the entry, and builds the requested index.
	ErrAbandonedInvalidIndex = errors.New("an abandoned invalid index with this name already exists on the target table")
	// ErrInvalidIndexOnOtherTable is returned (inside an *InvalidIndexError)
	// when the invalid index under the requested name sits on a different
	// table in the target schema, with no backend building it. A failed
	// build's debris carries the requested name whatever table it ended up
	// on, so the build refuses; but another table's debris is not this
	// change's to remove — the recovery refuses it too, and that table's
	// own change recovers it.
	ErrInvalidIndexOnOtherTable = errors.New("an invalid index with this name exists on a different table in the target schema")
	// ErrInvalidIndexNotDroppable is returned (inside an *InvalidIndexError)
	// when the invalid index under the requested name sits on the target
	// table but is not an index DROP INDEX CONCURRENTLY can remove: a
	// partitioned table's index (invalid by design until every partition
	// has an attached index, the documented CREATE INDEX ... ON ONLY
	// workflow), an index partition attached to such a parent, or an index
	// backing a constraint. None of these is the debris of a failed
	// concurrent build whatever its validity flag says, and no recovery
	// here can be proven through to a removal, so the build refuses and
	// the recovery leaves the entry exactly as it found it — only an
	// operator who knows what the index is can decide.
	ErrInvalidIndexNotDroppable = errors.New("an invalid index with this name exists on the target table and is not a droppable index: it is a partitioned table's index, an index partition, or backs a constraint")
	// ErrInvalidIndexBuilderUnobservable is returned (inside an
	// *InvalidIndexError) when an invalid index under the requested name
	// sits on the target table, no backend is visibly building it, and
	// this session cannot vouch for that silence: pg_stat_progress_create_index
	// withholds every column but the PID and database of another role's
	// command from a reader without pg_read_all_stats, and records nothing
	// at all for a session running with track_activities off. The entry may
	// be abandoned or may be a build in progress that this role cannot see,
	// so the build refuses without claiming either. The state is still
	// recoverable: RebuildAbandonedIndex proves abandonment under the table
	// lock every concurrent index command holds, which no visibility rule
	// can hide. An operator with pg_read_all_stats can classify it exactly.
	ErrInvalidIndexBuilderUnobservable = errors.New("an invalid index with this name exists on the target table and this role cannot observe whether a backend is building it")
	// ErrBuildLeftInvalidIndex is returned (inside an *InvalidIndexError)
	// when this executor's own build left an invalid catalog entry behind:
	// after a failure, once the build's backend provably stopped, or after
	// a reported success whose validity verification found the entry
	// invalid. The build never removes it in the same call: PostgreSQL
	// drops by name, not identity, so a drop without a fresh ownership
	// proof could destroy another actor's index registered under the same
	// name in the same window. The recovery is RebuildAbandonedIndex, which
	// carries that proof, or the operator's explicit DROP INDEX
	// CONCURRENTLY (see docs/invalid-index-recovery.md).
	ErrBuildLeftInvalidIndex = errors.New("the build left an invalid index behind")
	// ErrAbandonmentUnproven is returned (inside an *InvalidIndexError)
	// when a recovery could not carry its proof through to the removal:
	// the entry changed name or identity between two verification points,
	// or the drop left it in place. Nothing was destroyed; the state is
	// indeterminate and fails closed, and a later recovery starts over.
	ErrAbandonmentUnproven = errors.New("the invalid index could not be proven abandoned through to its removal")
	// ErrTargetIdentityChanged is returned (inside an *InvalidIndexError)
	// when the target table no longer resolves to the OID the build was
	// admitted against: it was dropped, replaced, or renamed while the
	// build ran. The catalog can no longer prove whether the failed build
	// left debris — the verdict is indeterminate, and indeterminate fails
	// closed.
	ErrTargetIdentityChanged = errors.New("the target table was dropped or replaced during the build: the catalog cannot prove whether the failed build left an invalid index")
	// ErrTableNotFound is returned when the statement's qualified table
	// resolves to nothing before the build starts; it distinguishes a
	// missing table from an inspection failure so callers can branch with
	// errors.Is instead of matching message text.
	ErrTableNotFound = errors.New("table not found")
	// ErrPoolTooSmall is returned at admission when the pool cannot hold
	// every session the operation needs at once — for a build, the build
	// session and the verdict session (buildMinConns); for a rebuild
	// recovery, its own session on top of those (recoveryMinConns); for a
	// drop-only recovery, its own session and one drop session
	// (dropRecoveryMinConns). The message names the sessions the refused
	// operation holds at its peak. The verdict is a correctness
	// dependency, not a nicety: without a reserved connection, every
	// failed build would resolve indeterminate, and a pool one connection
	// short would not fail but wait on itself for as long as the caller's
	// context allows. Like an unbounded budget, an unusable pool is
	// refused by construction.
	ErrPoolTooSmall = errors.New("the pool cannot hold every session the operation needs at once")
	// ErrCallerOwnedNeedsCancellableContext is returned when caller-owned
	// mode has no cancellation signal to bound the statement.
	ErrCallerOwnedNeedsCancellableContext = errors.New("a caller-owned build needs a cancellable context: with statement_timeout disabled the context is the statement's only bound")
	// ErrCallerOwnedOverallBudget is returned when a caller-owned budget
	// also carries a server deadline: the two bounds would contradict each
	// other, so the combination is refused before any session is acquired.
	ErrCallerOwnedOverallBudget = errors.New("a caller-owned build has no server deadline: Overall must be zero")
	// ErrUnboundedBudget is returned when the overall budget would leave
	// the statement without a working statement_timeout.
	ErrUnboundedBudget = errors.New("the overall budget would leave the statement unbounded")
	// ErrCancelledByCaller is returned when the build's statement was
	// cancelled because the caller's own context ended — cancelled or past
	// its deadline — while the statement ran. The cancellation reaches the
	// executor in one of two forms, the server's SQLSTATE 57014 or the
	// client's context error, and both are typed the same way: the cause is
	// the caller, in either budget mode — with one precedence in bounded
	// mode: a 57014 arriving once the overall budget has elapsed is the
	// budget's statement_timeout firing and is typed *BudgetError even if
	// the caller's context ended in the same instant, so the server's 57014
	// is the caller's only below the budget. An orchestrator reads this
	// error as its own lease lapsing, not as an operator's intervention
	// (ErrCancelledExternally).
	ErrCancelledByCaller = errors.New("the build was cancelled by its caller's context")
	// ErrCancelledExternally is returned when the build's statement was
	// cancelled (SQLSTATE 57014) while the caller's context was still live
	// and, in bounded mode, before its overall budget elapsed: neither the
	// caller nor the executor's statement_timeout can have done it, so the
	// cancellation came from outside — an operator's pg_cancel_backend, an
	// administrative tool. In caller-owned mode there is no server deadline,
	// so every 57014 under a live context is external. It is deliberately
	// not a *BudgetError: a budget exhaustion invites escalation to a
	// heavier strategy, while a deliberate cancel usually means the change
	// should be left alone.
	ErrCancelledExternally = errors.New("the build was cancelled from outside the executor")
)

// buildMinConns is the pool size a concurrent build needs: the build
