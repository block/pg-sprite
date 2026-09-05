// This file is the native concurrent index build: the first Phase 3
// native-path operation. CREATE INDEX CONCURRENTLY refuses to run inside a
// transaction block, so it cannot reuse the optimistic attempt's
// transactional path; it runs on a dedicated session under its own wait
// policy, and this executor owns the failure mode the statement is famous
// for — a failed build leaves a catalog entry marked invalid
// (pg_index.indisvalid = false) that every write still maintains but no
// query uses. The executor detects that leftover and surfaces it as a
// typed outcome that says whether the entry is another backend's build
// still in flight, this change's own or abandoned debris, or unprovable;
// the build itself never drops an index, because a name-based drop cannot
// prove whose index it destroys — that proof is the recovery's job (see
// recover.go).

package executor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
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
	ErrIfNotExistsUnsupported = errors.New("IF NOT EXISTS is not supported: a name-only no-op cannot prove the existing relation is the requested one, or even valid")
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
	// the build session and the verdict session at once. The verdict is a
	// correctness dependency, not a nicety: without a reserved second
	// connection, every failed build would resolve indeterminate. Like an
	// unbounded budget, an unusable verdict is refused by construction.
	ErrPoolTooSmall = errors.New("concurrent index build needs a pool of at least two connections: one for the build session, one reserved for the catalog verdict")
	// ErrCallerOwnedNeedsCancellableContext is returned when caller-owned
	// mode has no cancellation signal to bound the statement.
	ErrCallerOwnedNeedsCancellableContext = errors.New("a caller-owned build needs a cancellable context: with statement_timeout disabled the context is the statement's only bound")
	// ErrCancelledExternally is returned when the build's statement was
	// cancelled (SQLSTATE 57014) before its overall budget elapsed: the
	// executor's statement_timeout cannot have fired yet, so the
	// cancellation came from outside — an operator's pg_cancel_backend, an
	// administrative tool. It is deliberately not a *BudgetError: a budget
	// exhaustion invites escalation to a heavier strategy, while a
	// deliberate cancel usually means the change should be left alone.
	ErrCancelledExternally = errors.New("the build was cancelled from outside the executor before its budget elapsed")
)

// sessionCleanupTimeout bounds the client-side session housekeeping around
// a build: resetting the budget overrides and closing a session that must
// be discarded. It is deliberately separate from the build budget — a
// wedged socket must not hang the executor for the whole build budget.
const sessionCleanupTimeout = 5 * time.Second

// verdictTimeout bounds the post-failure catalog verdict: a backend-stop
// poll and one catalog snapshot, on a connection reserved before the build
// started. It is deliberately independent of the build budget — the
// verdict's work does not scale with the build's, so a build that fails in
// milliseconds must not hold its caller for an hours-long budget. The cost
// of the bound is one rare shape: a backend that outlives its client
// failure longer than this (a lost cancel signal) resolves indeterminate —
// fail-closed — instead of eventually proven.
const verdictTimeout = 30 * time.Second

// ConcurrentBudget bounds one CONCURRENTLY statement (index build or drop).
//
// CONCURRENTLY statements get their own wait policy instead of the blanket
// per-lock timeout: their waits for other transactions' snapshots are lock
// waits by implementation, so a session lock_timeout would cancel a healthy
// build mid-wait — and that cancellation is exactly what creates the invalid
// index this executor exists to prevent. The statement therefore runs with
// lock_timeout disabled and one bound on the whole statement: a server
// deadline, or in caller-owned mode the caller's cancellable context. That
// is safe with respect to the lock queue: the SHARE UPDATE EXCLUSIVE lock a
// concurrent build waits for does not block normal reads or writes queued
// behind it.
type ConcurrentBudget struct {
	// Overall bounds the whole statement, waits included, via
	// statement_timeout. It must be at least one millisecond (PostgreSQL's
	// granularity); expect index builds on large tables to need a generous
	// value.
	Overall time.Duration
	// CallerOwned makes the caller's cancellable context the statement's only
	// bound: the session runs with statement_timeout disabled and Overall must
	// be zero. The executor refuses a context that cannot be cancelled.
	CallerOwned bool
}

// maxOverallBudget is PostgreSQL's ceiling for statement_timeout (the
// setting is a signed 32-bit millisecond count); a larger value would be
// rejected — or worse, mis-set — by the server, leaving the statement
// unbounded.
const maxOverallBudget = time.Duration(math.MaxInt32) * time.Millisecond

// validate rejects budgets that would leave the statement unbounded.
func (b ConcurrentBudget) validate() error {
	if b.CallerOwned {
		// INV: LK-2 — the bound moves from the server timer to the caller's
		// cancellable context, checked before a session is acquired.
		if b.Overall != 0 {
			return fmt.Errorf("caller-owned budget requires Overall to be zero, got %s", b.Overall)
		}
		return nil
	}
	// INV: LK-2 — the build is bounded by construction; below one
	// millisecond the setting would round to zero, which disables
	// statement_timeout entirely.
	if b.Overall < time.Millisecond {
		return fmt.Errorf("overall budget must be at least 1ms, got %s", b.Overall)
	}
	if b.Overall > maxOverallBudget {
		return fmt.Errorf("overall budget must be at most %s, got %s", maxOverallBudget, b.Overall)
	}
	return nil
}

// IndexBuildReport says what a concurrent build did, machine-readably. It
// is returned only after the executor re-read the catalog and verified the
// built index is valid — it means "verified valid", not "the server did
// not complain" — so it is evidence an orchestrator can store or forward.
type IndexBuildReport struct {
	// Schema is the schema the index lives in, resolved from the target
	// table (an index is always created in its table's schema).
	Schema string `json:"schema"`
	// Index is the index name from the statement.
	Index string `json:"index"`
	// IndexOID is the verified index's catalog identity: the durable
	// handle a later reconciliation can use where the name alone could
	// have been reassigned.
	IndexOID uint32 `json:"index_oid"`
	// Duration is the wall-clock time of the build statement itself,
	// excluding session setup and the validity verification. It encodes
	// as integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
	// ServerVersion is the server_version of the PostgreSQL server that
	// ran the build.
	ServerVersion string `json:"server_version"`
}

// InvalidIndexError reports that an invalid index exists (or may remain)
// and the executor will not or cannot remove it: the one outcome that
// needs an operator. Build carries the failure that produced the leftover,
// nil when there is no build failure to carry (the entry predates this
// run, or a reported success failed its validity verification); Cleanup
// carries why automatic recovery was refused, failed, or could not be
// proven.
type InvalidIndexError struct {
	// Schema and Index identify the possibly-invalid index.
	Schema string
	Index  string
	// Table is the table the invalid index sits on, when the catalog
	// inspection saw the entry; empty when the state could not be
	// inspected.
	Table string
	// BuilderPID is the backend running a concurrent build or reindex of
	// this exact index (pg_stat_progress_create_index), when the entry is
	// in flight; zero otherwise.
	BuilderPID uint32
	// Build is the build failure that left the index invalid; nil when
	// there is no build failure to carry.
	Build error
	// Cleanup is why the recovery drop was refused, failed, or could not
	// be verified.
	Cleanup error
}

// Recoverable reports whether RebuildAbandonedIndex can remove the entry
// and build the requested index: true for this build's own proven
// leftover, for an abandoned entry on the target table, and for an entry
// on the target table whose builder this role cannot observe — the
// recovery's proof is a table lock, not the progress view, so it decides
// what the view could not. A visibly in-flight build, another table's
// debris, and an unproven state are not this actor's to recover.
func (e *InvalidIndexError) Recoverable() bool {
	switch {
	case errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex):
		return true
	case errors.Is(e.Cleanup, ErrAbandonedInvalidIndex):
		return true
	case errors.Is(e.Cleanup, ErrInvalidIndexBuilderUnobservable):
		return true
	default:
		return false
	}
}

// Error implements the error interface. The advice is as state-specific as
// the type: a removal is named only in the states where the recovery can
// prove ownership before it drops — the same standard the executor holds
// itself to. An in-flight build says wait; another table's debris and an
// unproven state get investigation steps, never a statement to
// copy-paste: the index under that name may be healthy.
func (e *InvalidIndexError) Error() string {
	name := fmt.Sprintf("%s.%s", e.Schema, e.Index)
	switch {
	case errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex):
		return fmt.Sprintf("index %s is this build's own invalid leftover; recover with RebuildAbandonedIndex or DROP INDEX CONCURRENTLY %s, see docs/invalid-index-recovery.md: %v",
			name, pgx.Identifier{e.Schema, e.Index}.Sanitize(), e.Cleanup)
	case errors.Is(e.Cleanup, ErrInvalidIndexBuildInFlight):
		return fmt.Sprintf("index %s is invalid because backend %d is still building it; wait for that build to finish or fail, see docs/invalid-index-recovery.md: %v",
			name, e.BuilderPID, e.Cleanup)
	case errors.Is(e.Cleanup, ErrAbandonedInvalidIndex):
		return fmt.Sprintf("index %s is abandoned invalid debris on table %s with no backend building it; recover with RebuildAbandonedIndex, see docs/invalid-index-recovery.md: %v",
			name, e.Table, e.Cleanup)
	case errors.Is(e.Cleanup, ErrInvalidIndexOnOtherTable):
		return fmt.Sprintf("index %s is invalid debris on a different table (%s), which this change will not remove; recover it from that table's own change or inspect it yourself, see docs/invalid-index-recovery.md: %v",
			name, e.Table, e.Cleanup)
	case errors.Is(e.Cleanup, ErrInvalidIndexBuilderUnobservable):
		return fmt.Sprintf("index %s is invalid on table %s and this role cannot see whether another backend is still building it; recover with RebuildAbandonedIndex, which proves the state under the table lock, or inspect pg_stat_progress_create_index as a role with pg_read_all_stats, see docs/invalid-index-recovery.md: %v",
			name, e.Table, e.Cleanup)
	default:
		return fmt.Sprintf("index %s may be invalid but the catalog state could not be proven; inspect pg_index.indisvalid and pg_stat_progress_create_index yourself before any recovery, see docs/invalid-index-recovery.md: %v",
			name, e.Cleanup)
	}
}

// Unwrap exposes the underlying failures to errors.Is/As.
func (e *InvalidIndexError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Build != nil {
		errs = append(errs, e.Build)
	}
	if e.Cleanup != nil {
		errs = append(errs, e.Cleanup)
	}
	return errs
}

// BuildIndexConcurrently runs one named CREATE INDEX ... CONCURRENTLY on a
// schema-qualified table, on its own session, outside any transaction,
// bounded by b. The pool must come from pkg/dbconn (raw pools may carry a
// reset baseline the session hygiene here cannot vouch for) and must allow
// at least two connections: the post-failure verdict's session is acquired
// up front, alongside the build session, so the verdict can never starve
// on a busy pool — a pool configured below two is refused at admission
// (ErrPoolTooSmall), and both acquisitions are bounded by ctx. The build
// never drops an index: PostgreSQL drops by name, not identity, so a drop
// here could not prove it is destroying this build's own debris rather
// than another actor's same-name index registered in the same window. That
// proof is RebuildAbandonedIndex's job — it holds the table lock every
// concurrent build needs while it renames the entry by identity and drops
// it by that unique name. Every invalid index the build meets is surfaced
// as a typed, fail-closed *InvalidIndexError whose Cleanup says which
// state it is (see docs/invalid-index-recovery.md):
//
//   - an invalid index with the requested name that another backend is
//     still building refuses to build and says wait
//     (ErrInvalidIndexBuildInFlight, carrying the builder's PID);
//   - an abandoned invalid index with the requested name on the target
//     table refuses to build and names the recovery
//     (ErrAbandonedInvalidIndex, Recoverable);
//   - an invalid index with the requested name on another table in the
//     schema refuses to build and leaves it to that table's own change
//     (ErrInvalidIndexOnOtherTable);
//   - an invalid index with the requested name on the target table whose
//     builder this role cannot observe — another role's progress row is
//     hidden without pg_read_all_stats, and none is recorded with
//     track_activities off — refuses to build without claiming it
//     abandoned (ErrInvalidIndexBuilderUnobservable, Recoverable: the
//     recovery's lock proof does not depend on the view);
//   - a failed build that left an invalid entry behind reports it
//     (ErrBuildLeftInvalidIndex, Recoverable) after proving the build's
//     own backend stopped, so the catalog verdict cannot race the dying
//     statement;
//   - a failed build that provably left nothing returns its failure alone
//     — a retry can start immediately.
//
// Cancellation by the server-owned overall budget surfaces as a *BudgetError; a
// cancellation arriving before the budget elapsed cannot be the budget's
// own statement_timeout and surfaces as ErrCancelledExternally instead.
// In caller-owned mode the cancellable context is the only bound and
// statement_timeout is disabled; SQLSTATE 57014 always surfaces as
// ErrCancelledExternally.
// Caller cancellation is a race: the client returns while the cancel signal
// travels to the server, so a build cancelled at the finish line may still
// complete. The guarantee is about the catalog, not the race: after this
// function returns without an *InvalidIndexError, the index is either
// valid or absent — success is returned only after re-reading the catalog
// and verifying pg_index.indisvalid on the built index, a guard against
// server-version drift in what "success" leaves behind.
//
// Unlike the optimistic attempt, no size-guard proof is required: the size
// guard exists because a blocking attempt holds ACCESS EXCLUSIVE for its
// whole budget, while a concurrent build takes only SHARE UPDATE EXCLUSIVE —
// long builds on large tables are its purpose.
func BuildIndexConcurrently(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget) (IndexBuildReport, error) {
	return buildIndexConcurrently(ctx, pool, sql, b, nil)
}

// BuildIndexConcurrentlyWithProgress runs a concurrent build while updating
// tracker. The caller may poll tracker concurrently with this blocking call.
func BuildIndexConcurrentlyWithProgress(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget, tracker *progress.Tracker) (rep IndexBuildReport, err error) {
	if tracker == nil {
		return rep, fmt.Errorf("%w: progress tracker is required", ErrInvariantViolation)
	}
	tracker.Start(1, progress.OperationConcurrentIndex)
	tracker.StartStep(1, progress.OperationConcurrentIndex, sql)
	defer func() { tracker.Finish(err) }()
	return buildIndexConcurrently(ctx, pool, sql, b, tracker)
}

func buildIndexConcurrently(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget, tracker *progress.Tracker) (IndexBuildReport, error) {
	var rep IndexBuildReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	// INV: LK-2 — caller-owned mode is bounded by the caller's cancellation
	// signal, so a context without one is refused before any session use.
	if b.CallerOwned && ctx.Done() == nil {
		return rep, ErrCallerOwnedNeedsCancellableContext
	}
	build, err := admitConcurrentIndexBuild(sql)
	if err != nil {
		return rep, err
	}
	// INV: LK-2 — the verdict is bounded by construction too: a pool that
	// cannot hold the build session and the verdict session at once would
	// make every failed build indeterminate.
	if pool.Config().MaxConns < 2 {
		return rep, ErrPoolTooSmall
	}

	// One session carries resolution, the pre-build inspection, and the
	// build itself, so the names the statement will resolve (search_path,
	// temporary schemas) are the names the executor inspected.
	conn, release, err := acquireBudgetedSession(ctx, pool, b)
	if err != nil {
		return rep, err
	}
	defer release()

	// The verdict session is reserved before the build starts, under the
	// pool's baseline budgets: the post-failure verdict is a correctness
	// dependency, and hoping a connection is free after an hours-long
	// build is not a reservation. Concurrent builds sharing one pool each
	// hold two connections; acquisition contention is bounded by ctx.
	verdictConn, err := pool.Acquire(ctx)
	if err != nil {
		return rep, fmt.Errorf("acquire verdict session: %w", err)
	}
	defer verdictConn.Release()

	target, err := resolveTarget(ctx, conn, build)
	if err != nil {
		return rep, err
	}
	rep.Schema, rep.Index = target.schema, build.index

	// Fail closed on pre-existing debris: an invalid index with this name
	// anywhere in this schema occupies the name, and after a failure of our
	// own the verdict could never tell it apart from our own leftover, so
	// the build refuses before touching anything — classified, so the
	// caller knows whether to wait, recover, or look elsewhere.
	existing, found, err := inspectInvalidIndex(ctx, conn, target.schema, build.index)
	if err != nil {
		return rep, err
	}
	if found {
		return rep, classifyInvalidIndex(existing, target, build.index)
	}

	// The backend PID anchors the post-failure ownership proof: recovery
	// waits for this backend to stop before trusting the catalog.
	pid := conn.Conn().PgConn().PID()
	if tracker != nil {
		tracker.SetConcurrentBuild(verdictConn, pid)
	}

	start := time.Now()
	if tracker != nil {
		start = tracker.Now()
	}
	_, buildErr := conn.Exec(ctx, sql)
	elapsed := elapsedSince(tracker, start)
	if tracker != nil {
		tracker.StopConcurrentBuild()
	}
	if buildErr == nil {
		return verifiedBuildReport(ctx, conn, build, target, elapsed)
	}
	buildErr = asConcurrentBudgetError(buildErr, b, elapsed, "concurrent index build")

	// Every failure gets the catalog verdict — no error is exempt. Even a
	// name-collision SQLSTATE cannot prove the statement created nothing:
	// index expressions and extension code run after the catalog entry
	// commits and can raise any SQLSTATE, name collisions included. The
	// verdict runs under its own bounded detached context, because the
	// build's context may be cancelled — and that cancellation may be
	// exactly why the build failed.
	verdictCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verdictTimeout)
	defer cancel()
	return rep, failedBuildVerdict(verdictCtx, verdictConn, build, target, pid, buildErr)
}

// verifiedBuildReport turns a build the server reported successful into
// evidence: it re-reads the catalog on the build's own session and returns
// a report only when the built index exists and is valid. The read closes
// a version-drift gap — today no admissible statement can succeed and
// leave an invalid entry (the concurrent partitioned-parent build, which
// does, is refused by the server), but that is a fact about current server
// versions, not about this executor's contract. A reported success that
// cannot be verified fails closed as an *InvalidIndexError.
func verifiedBuildReport(ctx context.Context, conn *pgxpool.Conn, build concurrentIndexBuild, target indexTarget, elapsed time.Duration) (IndexBuildReport, error) {
	rep := IndexBuildReport{Schema: target.schema, Index: build.index}
	var oid uint32
	var valid bool
	err := conn.QueryRow(ctx,
		`SELECT c.oid, i.indisvalid
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
		target.schema, build.index).Scan(&oid, &valid)
	if err != nil {
		return rep, &InvalidIndexError{Schema: target.schema, Index: build.index,
			Cleanup: fmt.Errorf("verify built index %s.%s: %w", target.schema, build.index, err)}
	}
	if !valid {
		return rep, &InvalidIndexError{Schema: target.schema, Index: build.index, Cleanup: ErrBuildLeftInvalidIndex}
	}
	rep.IndexOID = oid
	rep.Duration = elapsed
	rep.ServerVersion = conn.Conn().PgConn().ParameterStatus("server_version")
	return rep, nil
}

// failedBuildVerdict decides, read-only, what a failed build left behind: a
// build that failed after creating its catalog entry leaves the entry
// marked invalid; one that failed before that point (for example while
// waiting for its table lock) leaves nothing. The verdict reports, never
// drops, and fails closed — every step short of a provably clean catalog
// wraps the build failure in an *InvalidIndexError:
//
//  1. Prove the build's own backend stopped. A client-side failure returns
//     before the server acts on it, and an inspection racing the
//     still-running backend could wrongly report a clean catalog. Once the
//     backend has stopped, its catalog effect is final.
//  2. Read one catalog snapshot — a single statement, so the identity
//     proof and the index inspection cannot straddle a concurrent
//     replacement — and trust it only whole (see catalogVerdict).
func failedBuildVerdict(ctx context.Context, q querier, build concurrentIndexBuild, target indexTarget, pid uint32, buildErr error) error {
	if err := awaitBackendStopped(ctx, q, pid); err != nil {
		return &InvalidIndexError{Schema: target.schema, Index: build.index, Build: buildErr, Cleanup: err}
	}
	return catalogVerdict(ctx, q, build, target, buildErr)
}

// catalogVerdict reads one catalog snapshot and decides what the failed
// build left behind. The snapshot is a single statement carrying two facts
// that must be read together:
//
//   - what the resolved (schema, table) name currently identifies, which
//     must be the pinned OID the build was admitted against — a dropped or
//     replaced table means any debris lives on a relation this verdict
//     never resolved, so the verdict is indeterminate, not clean;
//   - whether an index with the build's name exists in the target schema
//     and is invalid, and whether a backend is still building it. The name
//     is checked schema-wide, not pinned to the table OID: a failed build's
//     debris carries the requested name in the table's schema, and pinning
//     to the OID would go blind if the table was swapped under the same
//     name while the build ran. The builder check keeps the ownership
//     claim honest: the build's own backend has provably stopped by now, so
//     a live builder on the invalid entry is another actor whose build
//     won the name before ours created anything — the entry is theirs, in
//     flight, not our leftover. The claim is made only when this session
//     could have seen such a builder: with another role's progress row
//     hidden, or activity tracking off, the entry is reported without an
//     owner.
//
// All facts are ordinary catalog scans inside one SELECT, so they share
// the statement's MVCC snapshot; name-resolution helpers like to_regclass
// are deliberately absent — they read through the relation cache's own
// separately refreshed catalog snapshot, which would let the identity
// proof and the index inspection straddle a concurrent replacement.
//
// Every outcome short of a provably clean catalog wraps the build failure
// in an *InvalidIndexError; a clean catalog returns the build failure
// alone.
func catalogVerdict(ctx context.Context, q querier, build concurrentIndexBuild, target indexTarget, buildErr error) error {
	fail := func(cleanup error) error {
		return &InvalidIndexError{Schema: target.schema, Index: build.index, Build: buildErr, Cleanup: cleanup}
	}
	var (
		currentOID *uint32
		indexValid *bool
		builderPID *int32
		hiddenRows int64
		tracking   bool
	)
	err := q.QueryRow(ctx,
		`SELECT (SELECT c.oid
		           FROM pg_catalog.pg_class c
		           JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		          WHERE n.nspname OPERATOR(pg_catalog.=) $1
		            AND c.relname OPERATOR(pg_catalog.=) $2),
		        (SELECT i.indisvalid
		           FROM pg_catalog.pg_index i
		           JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		           JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		          WHERE n.nspname OPERATOR(pg_catalog.=) $1
		            AND c.relname OPERATOR(pg_catalog.=) $3),
		        (SELECT `+builderPIDSubquery+`
		           FROM pg_catalog.pg_class c
		           JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		          WHERE n.nspname OPERATOR(pg_catalog.=) $1
		            AND c.relname OPERATOR(pg_catalog.=) $3),
		        `+builderVisibilityColumns,
		target.schema, build.table, build.index).Scan(&currentOID, &indexValid, &builderPID, &hiddenRows, &tracking)
	if err != nil {
		return fail(fmt.Errorf("inspect index %s.%s: %w", target.schema, build.index, err))
	}
	if currentOID == nil || *currentOID != target.tableOID {
		return fail(ErrTargetIdentityChanged)
	}
	// No index under the name, or a valid one: either way the failed build
	// left no invalid entry, and the failure stands alone.
	if indexValid == nil || *indexValid {
		return buildErr
	}
	builder := newBuilderFacts(builderPID, hiddenRows, tracking)
	owned := &InvalidIndexError{Schema: target.schema, Index: build.index, Table: build.table,
		BuilderPID: builder.pid, Build: buildErr}
	switch {
	case builder.pid != 0:
		owned.Cleanup = ErrInvalidIndexBuildInFlight
	case !builder.observable():
		owned.Cleanup = ErrInvalidIndexBuilderUnobservable
	default:
		owned.Cleanup = ErrBuildLeftInvalidIndex
	}
	return owned
}

// concurrentIndexBuild is the admitted statement's identity facts.
type concurrentIndexBuild struct {
	index       string
	tableSchema string
	table       string
}

// admitConcurrentIndexBuild is the executor's own admission check: exactly
// one statement, a CREATE INDEX, carrying CONCURRENTLY, with a name, on a
// schema-qualified table. Each refusal is typed; nothing is executed on a
// refusal.
func admitConcurrentIndexBuild(sql string) (concurrentIndexBuild, error) {
	var build concurrentIndexBuild
	st, err := statement.ParseOne(sql)
	if err != nil {
		return build, err
	}
	if st.Kind() != statement.KindCreateIndex {
		return build, fmt.Errorf("%w: got %s", ErrNotConcurrentIndexBuild, st.Kind())
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return build, err
	}
	// A CREATE INDEX statement yields exactly one operation; anything else
	// is a parse-boundary version skew and is refused rather than guessed.
	if len(ops) != 1 || ops[0].Kind != statement.OpCreateIndex {
		return build, ErrNotConcurrentIndexBuild
	}
	if !ops[0].Concurrent {
		return build, fmt.Errorf("%w: the statement is a plain CREATE INDEX", ErrNotConcurrentIndexBuild)
	}
	if ops[0].Name == "" {
		return build, ErrUnnamedIndex
	}
	if ops[0].IfNotExists {
		return build, ErrIfNotExistsUnsupported
	}
	if st.Schema() == "" {
		return build, ErrUnqualifiedTable
	}
	return concurrentIndexBuild{index: ops[0].Name, tableSchema: st.Schema(), table: st.Table()}, nil
}

// indexTarget is the resolved identity the recovery paths key on: the
// schema scopes the index-name inspections, and the table OID is the
// identity the post-failure verdict re-proves — a table swapped under the
// same name makes the verdict indeterminate rather than clean.
type indexTarget struct {
	tableOID uint32
	schema   string
}

// querier is the query surface the catalog helpers need; *pgxpool.Conn and
// *pgxpool.Pool both satisfy it.
//
// Every proof query these helpers run names its relations, functions, and
// operators with an explicit pg_catalog qualification: search_path may
// legitimately list a user schema before pg_catalog, and a user relation
// named pg_index — or a user operator named = — would silently shadow the
// catalog and turn a fail-closed proof into a false clean. Only the
// admitted build statement itself uses the session's normal resolution.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// resolveTarget resolves the statement's qualified table to its OID and
// schema, proving it exists. Admission guarantees qualification, so the
// resolution is session-independent — the same name yields the same table
// on the build session and on the verdict's pool session alike.
func resolveTarget(ctx context.Context, q querier, build concurrentIndexBuild) (indexTarget, error) {
	ref := pgx.Identifier{build.tableSchema, build.table}
	var target indexTarget
	err := q.QueryRow(ctx,
		`SELECT c.oid, n.nspname
		   FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)`,
		ref.Sanitize()).Scan(&target.tableOID, &target.schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, fmt.Errorf("resolve table %s: %w", ref.Sanitize(), ErrTableNotFound)
	}
	if err != nil {
		return target, fmt.Errorf("resolve table %s: %w", ref.Sanitize(), err)
	}
	return target, nil
}

// invalidIndex is one catalog observation of an invalid index: its
// identity (the OID a later proof re-checks), the table it sits on, and
// what the snapshot says about a backend building it.
type invalidIndex struct {
	oid      uint32
	tableOID uint32
	table    string
	builder  builderFacts
}

// builderFacts is what one catalog snapshot says about the backend building
// an index, together with whether that answer can be trusted. A visible
// builder is positive evidence; its absence is evidence only when this
// session could have seen one.
type builderFacts struct {
	// pid is the backend whose concurrent build or reindex is reported
	// against the index OID by pg_stat_progress_create_index; zero when
	// none is visible.
	pid uint32
	// hiddenRows counts the concurrent index commands in this database
	// whose target the progress view withholds from this session: another
	// role's command shows only its PID and database to a reader without
	// pg_read_all_stats. Any such row may be the builder of the entry
	// under inspection.
	hiddenRows int64
	// tracking reports this session's track_activities. A backend records
	// no progress row while the setting is off, so an off setting here —
	// the same server default the builder most likely runs under — means
	// the view's silence proves nothing.
	tracking bool
}

// observable reports whether a zero pid means no backend is building the
// entry: every command in the database is visible to this session and
// commands are being recorded at all.
func (f builderFacts) observable() bool {
	return f.tracking && f.hiddenRows == 0
}

// builderPIDSubquery is the scalar subquery that names the backend
// building the index whose pg_class row is aliased c. PostgreSQL reports
// a concurrent build's index OID in pg_stat_progress_create_index as soon
// as the catalog entry is visible, and the row disappears when the
// command ends, so a NULL here means no backend is visibly building the
// entry right now — builderVisibilityColumns says whether that visibility
// can be trusted. It is scoped to the current database because progress
// rows are cluster-wide while OIDs are not.
const builderPIDSubquery = `(SELECT p.pid
		           FROM pg_catalog.pg_stat_progress_create_index p
		          WHERE p.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()
		            AND p.index_relid OPERATOR(pg_catalog.=) c.oid
		          ORDER BY p.pid
		          LIMIT 1)`

// builderVisibilityColumns are the two facts that say whether
// builderPIDSubquery's silence is trustworthy, read in the same snapshot:
// the number of progress rows in this database whose target relation the
// view withholds from this session (a visible command always reports its
// table, so a NULL there is a hidden command, not a command that has yet
// to create its index), and whether this session records activity at all.
const builderVisibilityColumns = `(SELECT pg_catalog.count(*)
		           FROM pg_catalog.pg_stat_progress_create_index p
		          WHERE p.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()
		            AND p.relid IS NULL),
		        pg_catalog.current_setting('track_activities') OPERATOR(pg_catalog.=) 'on'`

// newBuilderFacts assembles the scanned builder columns. Backend PIDs are
// positive, so the nullable-PID conversion is lossless.
func newBuilderFacts(pid *int32, hiddenRows int64, tracking bool) builderFacts {
	return builderFacts{pid: pidValue(pid), hiddenRows: hiddenRows, tracking: tracking}
}

// inspectInvalidIndex reports whether an index named index exists anywhere
// in the schema and is marked invalid (pg_index.indisvalid = false) — the
// state a failed concurrent build leaves behind, and the state a
// concurrent build in progress is in. The check is deliberately
// schema-wide rather than pinned to one table: debris carries the
// requested name in the table's schema whatever table it ended up on, and
// an invalid index under this name on any table makes a later failure
// verdict undecidable. The identity, table, and builder facts come from
// one statement, so they describe the same entry.
func inspectInvalidIndex(ctx context.Context, q querier, schema, index string) (invalidIndex, bool, error) {
	var (
		found      invalidIndex
		valid      bool
		builderPID *int32
		hiddenRows int64
		tracking   bool
	)
	err := q.QueryRow(ctx,
		`SELECT c.oid, i.indisvalid, i.indrelid, t.relname, `+builderPIDSubquery+`, `+builderVisibilityColumns+`
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
		schema, index).Scan(&found.oid, &valid, &found.tableOID, &found.table, &builderPID, &hiddenRows, &tracking)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidIndex{}, false, nil
	}
	if err != nil {
		return invalidIndex{}, false, fmt.Errorf("inspect index %s.%s: %w", schema, index, err)
	}
	if valid {
		return invalidIndex{}, false, nil
	}
	found.builder = newBuilderFacts(builderPID, hiddenRows, tracking)
	return found, true, nil
}

// pidValue turns a nullable backend PID into the zero-means-none form the
// error types carry. Backend PIDs are positive, so the conversion is
// lossless.
func pidValue(pid *int32) uint32 {
	if pid == nil || *pid <= 0 {
		return 0
	}
	return uint32(*pid)
}

// classifyInvalidIndex turns an observed invalid index under the requested
// name into the typed refusal the build and the recovery both act on. The
// order is the order of proof strength: a visible builder is positive
// evidence the entry is not abandoned whatever table it is on; without one,
// the table decides whether the debris is this change's to recover; and on
// the target table, the entry is called abandoned only when this session
// could have seen a builder and saw none.
func classifyInvalidIndex(existing invalidIndex, target indexTarget, index string) *InvalidIndexError {
	e := &InvalidIndexError{Schema: target.schema, Index: index, Table: existing.table, BuilderPID: existing.builder.pid}
	switch {
	case existing.builder.pid != 0:
		e.Cleanup = ErrInvalidIndexBuildInFlight
	case existing.tableOID != target.tableOID:
		e.Cleanup = ErrInvalidIndexOnOtherTable
	case !existing.builder.observable():
		e.Cleanup = ErrInvalidIndexBuilderUnobservable
	default:
		e.Cleanup = ErrAbandonedInvalidIndex
	}
	return e
}

// awaitBackendStopped waits until the given backend provably stopped
// executing: it disconnected, or pg_stat_activity positively reports it
// idle (in or out of a transaction). It is the catalog verdict's first
// proof — a client-side failure (cancellation, connection loss) returns
// before the server acts on it, and an inspection racing the still-running
// build could wrongly report a clean catalog while the build goes on to
// create its invalid entry. Anything short of positive evidence fails
// closed: a state that means the statement may still be running keeps
// polling until ctx expires, and a state with no visibility (activity
// tracking disabled, hidden backend) refuses immediately — it will never
// become provable by waiting.
func awaitBackendStopped(ctx context.Context, q querier, pid uint32) error {
	const pollInterval = 50 * time.Millisecond
	for {
		var state *string
		err := q.QueryRow(ctx,
			`SELECT state FROM pg_catalog.pg_stat_activity WHERE pid OPERATOR(pg_catalog.=) $1`,
			int64(pid)).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			// The backend is gone; a disconnected backend runs nothing.
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect build backend %d: %w", pid, err)
		}
		switch classifyBackendState(state) {
		case backendStopped:
			return nil
		case backendRunning:
			// The statement may still be running; keep polling.
		case backendUnprovable:
			return fmt.Errorf("build backend %d reports state %s: cannot prove the statement stopped", pid, describeState(state))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("build backend %d is still executing: %w", pid, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// backendVerdict is the classification of one pg_stat_activity state
// observation.
type backendVerdict int

const (
	// backendStopped means the backend has positively stopped executing.
	backendStopped backendVerdict = iota
	// backendRunning means the statement may still be executing; the
	// observation is worth repeating.
	backendRunning
	// backendUnprovable means no amount of waiting turns the observation
	// into proof: activity tracking is off, the state is hidden, or the
	// state is one this executor does not know.
	backendUnprovable
)

// classifyBackendState maps one observed pg_stat_activity state to its
// verdict. The cases are the documented pg_stat_activity.state vocabulary
// (PostgreSQL 14–18): the idle states, "active" and "fastpath function
// call" (may still be executing), NULL (hidden backend), and "disabled"
// (track_activities off). Only positive evidence counts as stopped;
// NULL, "disabled", and any state outside the vocabulary are unprovable —
// treating them as stopped would let the verdict race a build that is in
// fact still running.
func classifyBackendState(state *string) backendVerdict {
	if state == nil {
		return backendUnprovable
	}
	switch *state {
	case "idle", "idle in transaction", "idle in transaction (aborted)":
		return backendStopped
	case "active", "fastpath function call":
		return backendRunning
	default:
		return backendUnprovable
	}
}

// describeState renders an observed state for an error message.
func describeState(state *string) string {
	if state == nil {
		return "NULL"
	}
	return strconv.Quote(*state)
}

// acquireBudgetedSession acquires one pooled session and applies the
// CONCURRENTLY wait policy to it. CONCURRENTLY statements refuse to run
// inside a transaction block, which also rules out SET LOCAL, so the
// overrides are session-level: the returned release resets them before the
// session goes back to the pool and discards the session when the reset
// cannot be proven.
func acquireBudgetedSession(ctx context.Context, pool *pgxpool.Pool, b ConcurrentBudget) (*pgxpool.Conn, func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire session: %w", err)
	}

	// INV: LK-2 — the CONCURRENTLY exception policy: no per-lock timeout
	// (a lock_timeout would cancel the statement's snapshot waits and leave
	// the invalid index this executor exists to prevent); either one overall
	// statement deadline or caller cancellation bounds the build instead. A bare integer is
	// milliseconds to PostgreSQL; the settings are applied here regardless
	// of the pool's defaults.
	statementTimeout := b.Overall.Milliseconds()
	if b.CallerOwned {
		statementTimeout = 0
	}
	budgets := "SET lock_timeout = 0; SET statement_timeout = " + strconv.FormatInt(statementTimeout, 10)
	if _, err := conn.Exec(ctx, budgets); err != nil {
		// The two SETs may have partially applied — PostgreSQL runs a
		// simple-query batch statement by statement — so the session must
		// not return to the pool: releasing it would hand lock_timeout = 0
		// to an unsuspecting borrower. Hijacking detaches it from the pool
		// structurally; the close error only restates that the connection
		// is already unusable, and the Exec failure is the one returned.
		discardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		_ = conn.Hijack().Close(discardCtx)
		return nil, nil, fmt.Errorf("set session budgets: %w", err)
	}

	release := func() {
		// Housekeeping runs even when ctx is cancelled — a cancelled build
		// still used this session — under its own short bound: a wedged
		// socket must not hang the executor.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		if _, err := conn.Exec(cleanupCtx, "RESET lock_timeout; RESET statement_timeout"); err == nil {
			conn.Release()
			return
		}
		// The reset could not be proven, so the session must not be
		// reused: hijacking detaches it from the pool structurally, and
		// the close error only means the connection is already unusable.
		_ = conn.Hijack().Close(cleanupCtx)
	}
	return conn, release, nil
}

// asConcurrentBudgetError types a CONCURRENTLY statement's failure; the
// operation names the statement in the operational-failure wrapping.
// SQLSTATE 57014 is query_canceled generally, not statement_timeout
// specifically — an operator's pg_cancel_backend raises the same code — so
// the elapsed time corroborates: the budget's own statement_timeout cannot
// fire before the budget elapses, so a 57014 arriving earlier is an
// external cancellation (ErrCancelledExternally), not budget exhaustion.
// The boundary is approximate by network latency — a cancel landing within
// that sliver of the deadline reads as the budget — but the two truths
// coincide there. Anything else is wrapped as an operational failure.
func asConcurrentBudgetError(err error, b ConcurrentBudget, elapsed time.Duration, operation string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateQueryCanceled {
		if b.CallerOwned {
			return fmt.Errorf("%w (after %s, caller-owned deadline): %w",
				ErrCancelledExternally, elapsed.Round(time.Millisecond), err)
		}
		if elapsed >= b.Overall {
			return &BudgetError{Cause: CauseStatement, Budget: b.Overall}
		}
		return fmt.Errorf("%w (after %s of a %s budget): %w",
			ErrCancelledExternally, elapsed.Round(time.Millisecond), b.Overall, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
