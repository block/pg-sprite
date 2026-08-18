// This file is the native concurrent index build: the first Phase 3
// native-path operation. CREATE INDEX CONCURRENTLY refuses to run inside a
// transaction block, so it cannot reuse the optimistic attempt's
// transactional path; it runs on a dedicated session under its own wait
// policy, and this executor owns the failure mode the statement is famous
// for — a failed build leaves a catalog entry marked invalid
// (pg_index.indisvalid = false) that every write still maintains but no
// query uses. The executor detects that leftover and surfaces it as a
// typed outcome naming the operator's explicit recovery; it never drops an
// index itself, because a name-based drop cannot prove whose index it
// destroys.

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
	// ErrIfNotExistsUnsupported is returned for CREATE INDEX CONCURRENTLY
	// IF NOT EXISTS. The clause checks only the name: it succeeds as a
	// no-op while an invalid or unrelated index owns that name, so the
	// executor could report success over an index it cannot vouch for.
	ErrIfNotExistsUnsupported = errors.New("CREATE INDEX CONCURRENTLY IF NOT EXISTS is not supported: a name-only no-op cannot prove the existing index is valid or even the requested one")
	// ErrPreexistingInvalidIndex is returned when an invalid index with the
	// requested name already exists in the target schema — on any table.
	// The executor cannot prove who owns that entry — an in-progress
	// concurrent build by another actor is invalid until it finishes — and
	// after a failure of its own it could never distinguish that entry
	// from its own leftover, so it refuses to build. This state is not a
	// drop instruction: the index may be healthy and mid-build (see
	// docs/invalid-index-recovery.md).
	ErrPreexistingInvalidIndex = errors.New("an invalid index with this name already exists in the target schema")
	// ErrBuildLeftInvalidIndex is returned (inside an *InvalidIndexError)
	// when this executor's own build left an invalid catalog entry behind:
	// after a failure, once the build's backend provably stopped, or after
	// a reported success whose validity verification found the entry
	// invalid. The executor never removes it automatically: PostgreSQL
	// drops by name, not identity, so an automatic drop could destroy
	// another actor's index registered under the same name in the same
	// window. The recovery is the operator's explicit DROP INDEX
	// CONCURRENTLY (see docs/invalid-index-recovery.md).
	ErrBuildLeftInvalidIndex = errors.New("the build left an invalid index behind")
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
// lock_timeout disabled and one overall deadline. That is safe with respect
// to the lock queue: the SHARE UPDATE EXCLUSIVE lock a concurrent build
// waits for does not block normal reads or writes queued behind it.
type ConcurrentBudget struct {
	// Overall bounds the whole statement, waits included, via
	// statement_timeout. It must be at least one millisecond (PostgreSQL's
	// granularity); expect index builds on large tables to need a generous
	// value.
	Overall time.Duration
}

// maxOverallBudget is PostgreSQL's ceiling for statement_timeout (the
// setting is a signed 32-bit millisecond count); a larger value would be
// rejected — or worse, mis-set — by the server, leaving the statement
// unbounded.
const maxOverallBudget = time.Duration(math.MaxInt32) * time.Millisecond

// validate rejects budgets that would leave the statement unbounded.
func (b ConcurrentBudget) validate() error {
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
	// Build is the build failure that left the index invalid; nil when
	// there is no build failure to carry.
	Build error
	// Cleanup is why the recovery drop was refused, failed, or could not
	// be verified.
	Cleanup error
}

// Error implements the error interface. The advice is as state-specific as
// the type: a name-based drop is named only when the entry is proven this
// build's own leftover (ErrBuildLeftInvalidIndex) — the same ownership
// standard the executor holds itself to when it refuses to drop
// automatically. An unproven or uninspected state gets investigation
// steps, never a statement to copy-paste: the index under that name may be
// healthy, or another actor's build still in progress.
func (e *InvalidIndexError) Error() string {
	name := fmt.Sprintf("%s.%s", e.Schema, e.Index)
	switch {
	case errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex):
		return fmt.Sprintf("index %s is this build's own invalid leftover; recover with DROP INDEX CONCURRENTLY %s, see docs/invalid-index-recovery.md: %v",
			name, pgx.Identifier{e.Schema, e.Index}.Sanitize(), e.Cleanup)
	case errors.Is(e.Cleanup, ErrPreexistingInvalidIndex):
		return fmt.Sprintf("index %s is invalid but not proven abandoned — it may be another actor's build still in progress; check pg_stat_activity for a running CREATE INDEX CONCURRENTLY before any recovery, see docs/invalid-index-recovery.md: %v",
			name, e.Cleanup)
	default:
		return fmt.Sprintf("index %s may be invalid but the catalog state could not be proven; inspect pg_index.indisvalid and pg_stat_activity yourself before any recovery, see docs/invalid-index-recovery.md: %v",
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
// (ErrPoolTooSmall), and both acquisitions are bounded by ctx. It never
// drops an index: PostgreSQL drops by name, not identity, so no automatic drop can
// prove it is destroying this build's own debris rather than another
// actor's same-name index registered in the same window (the engine's
// one-migration-per-table lease — planned invariant LK-1 — would close
// that; once it exists this API should demand its proof). Every invalid
// index is instead surfaced as a typed, fail-closed outcome carrying
// state-specific operator guidance (see docs/invalid-index-recovery.md):
//
//   - a pre-existing invalid index with the requested name refuses to
//     build (ErrPreexistingInvalidIndex);
//   - a failed build that left an invalid entry behind reports it
//     (ErrBuildLeftInvalidIndex) after proving the build's own backend
//     stopped, so the catalog verdict cannot race the dying statement;
//   - a failed build that provably left nothing returns its failure alone
//     — a retry can start immediately.
//
// Cancellation by the overall budget surfaces as a *BudgetError; a
// cancellation arriving before the budget elapsed cannot be the budget's
// own statement_timeout and surfaces as ErrCancelledExternally instead.
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
	tracker.StartStep(1, progress.OperationConcurrentIndex)
	defer func() { tracker.Finish(err) }()
	return buildIndexConcurrently(ctx, pool, sql, b, tracker)
}

func buildIndexConcurrently(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget, tracker *progress.Tracker) (IndexBuildReport, error) {
	var rep IndexBuildReport
	if err := b.validate(); err != nil {
		return rep, err
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
	// anywhere in this schema cannot be proven ours — it may be another
	// actor's in-progress build — and after a failure of our own the
	// verdict could never tell it apart from our own leftover, so the
	// build refuses before touching anything.
	leftover, err := invalidIndexByName(ctx, conn, target.schema, build.index)
	if err != nil {
		return rep, err
	}
	if leftover {
		return rep, &InvalidIndexError{Schema: target.schema, Index: build.index, Cleanup: ErrPreexistingInvalidIndex}
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
	buildErr = asConcurrentBudgetError(buildErr, b, elapsed)

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
//     and is invalid. The name is checked schema-wide, not pinned to the
//     table OID: a failed build's debris carries the requested name in the
//     table's schema, and pinning to the OID would go blind if the table
//     was swapped under the same name while the build ran.
//
// Both facts are ordinary catalog scans inside one SELECT, so they share
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
	var currentOID *uint32
	var indexValid *bool
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
		            AND c.relname OPERATOR(pg_catalog.=) $3)`,
		target.schema, build.table, build.index).Scan(&currentOID, &indexValid)
	if err != nil {
		return fail(fmt.Errorf("inspect index %s.%s: %w", target.schema, build.index, err))
	}
	if currentOID == nil || *currentOID != target.tableOID {
		return fail(ErrTargetIdentityChanged)
	}
	if indexValid != nil && !*indexValid {
		return fail(ErrBuildLeftInvalidIndex)
	}
	return buildErr
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

// invalidIndexByName reports whether an index named index exists anywhere
// in the schema and is marked invalid (pg_index.indisvalid = false) — the
// state a failed concurrent build leaves behind. The check is deliberately
// schema-wide rather than pinned to one table: debris carries the requested
// name in the table's schema whatever table it ended up on, and an invalid
// index under this name on any table makes a later failure verdict
// undecidable. An invalid index cannot become valid on its own, so a true
// answer stays true until someone drops or rebuilds it.
func invalidIndexByName(ctx context.Context, q querier, schema, index string) (bool, error) {
	var valid bool
	err := q.QueryRow(ctx,
		`SELECT i.indisvalid
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
		schema, index).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect index %s.%s: %w", schema, index, err)
	}
	return !valid, nil
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
	// the invalid index this executor exists to prevent); one overall
	// statement deadline bounds every statement instead. A bare integer is
	// milliseconds to PostgreSQL; the settings are applied here regardless
	// of the pool's defaults.
	budgets := "SET lock_timeout = 0; SET statement_timeout = " +
		strconv.FormatInt(b.Overall.Milliseconds(), 10)
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

// asConcurrentBudgetError types the build failure. SQLSTATE 57014 is
// query_canceled generally, not statement_timeout specifically — an
// operator's pg_cancel_backend raises the same code — so the elapsed build
// time corroborates: the budget's own statement_timeout cannot fire before
// the budget elapses, so a 57014 arriving earlier is an external
// cancellation (ErrCancelledExternally), not budget exhaustion. The
// boundary is approximate by network latency — a cancel landing within
// that sliver of the deadline reads as the budget — but the two truths
// coincide there. Anything else is wrapped as an operational failure.
func asConcurrentBudgetError(err error, b ConcurrentBudget, elapsed time.Duration) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateQueryCanceled {
		if elapsed >= b.Overall {
			return &BudgetError{Cause: CauseStatement, Budget: b.Overall}
		}
		return fmt.Errorf("%w (after %s of a %s budget): %w",
			ErrCancelledExternally, elapsed.Round(time.Millisecond), b.Overall, err)
	}
	return fmt.Errorf("concurrent index build: %w", err)
}
