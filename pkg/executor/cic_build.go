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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

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
// A cancelled build is typed by its cause. A server cancellation at or
// past the server-owned overall budget is that budget's own
// statement_timeout and surfaces as a *BudgetError, whatever the caller's
// context did meanwhile. Below the budget, the caller's own context ending
// surfaces as ErrCancelledByCaller in either mode, and a cancellation
// under a live context cannot be the budget's or the caller's and
// surfaces as ErrCancelledExternally. In caller-owned mode the cancellable
// context is the only bound and statement_timeout is disabled, so a 57014
// under a live context is always ErrCancelledExternally.
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
	if pool.Config().MaxConns < buildMinConns {
		return rep, fmt.Errorf("concurrent index build needs %d connections (build session and reserved verdict session), pool holds %d: %w",
			buildMinConns, pool.Config().MaxConns, ErrPoolTooSmall)
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
		return rep, classifyInvalidIndex(existing, target, build.index, ErrAbandonedInvalidIndex)
	}
	// Quarantined debris on the target table refuses the build too, loudly:
	// a recovery that renamed an entry and then died has freed the
	// requested name, and a build that silently succeeded beside the
	// leftover would be the last time anyone heard of it. The refusal is
	// classified against the quarantine name — the entry that is invalid —
	// and RebuildAbandonedIndex with this same statement sweeps it. Entries
	// the server will not drop concurrently are not refused here: they are
	// not this build's debris, the recovery reports them as skipped, and a
	// permanent refusal would wedge every later build on the table.
	debris, err := listDroppableQuarantinedIndexes(ctx, conn, target)
	if err != nil {
		return rep, err
	}
	if len(debris) != 0 {
		return rep, classifyInvalidIndex(debris[0], target, quarantineName(debris[0].oid), ErrAbandonedInvalidIndex)
	}

	// The backend PID anchors the post-failure ownership proof: recovery
	// waits for this backend to stop before trusting the catalog.
	pid := conn.Conn().PgConn().PID()
	if tracker != nil {
		tracker.SetConcurrentBuild(verdictConn, pid)
		// The build's PID stays a cancel target only while this session
		// owns the backend. The explicit call below retires it before the
		// verdict so no poll can share the verdict session; the deferred
		// call guarantees retirement on every exit, including a panic.
		defer tracker.StopConcurrentBuild()
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

	// Both verdicts run under their own bounded detached context: the
	// build's context may have ended — that may be exactly why the build
	// failed — and a build that succeeded at the finish line must not
	// report as unproven because the caller cancelled a moment later.
	verdictCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verdictTimeout)
	defer cancel()
	if buildErr == nil {
		return verifiedBuildReport(verdictCtx, conn, build, target, elapsed)
	}
	buildErr = asConcurrentBudgetError(ctx, buildErr, b, elapsed, "concurrent index build")

	// Every failure gets the catalog verdict — no error is exempt. Even a
	// name-collision SQLSTATE cannot prove the statement created nothing:
	// index expressions and extension code run after the catalog entry
	// commits and can raise any SQLSTATE, name collisions included.
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
//     and is invalid, which table it sits on, whether the server could
//     drop it concurrently, and whether a backend is still building it.
//     The name is checked schema-wide, not pinned to the table OID: a
//     failed build's debris carries the requested name in the table's
//     schema, and pinning to the OID would go blind if the table was
//     swapped under the same name while the build ran. The facts then
//     go through the same classifier as the pre-build inspection, with
//     one difference: an observable silence on the target table is this
//     build's own leftover, not anonymous abandoned debris. The other
//     verdicts keep the ownership claim honest. The build's own backend
//     has provably stopped by now, so a live builder on the invalid entry
//     is another actor whose build won the name before ours created
//     anything — theirs, in flight. An entry on a different table is not
//     provably ours either: our build may have created it on a table that
//     briefly owned the resolved name, or another actor's failed build
//     may have — the catalog cannot tell, and an ownership claim over a
//     foreign table's index is exactly the claim the recovery refuses to
//     act on, so the verdict reports the entry where it is and claims
//     nothing. The ownership claim is made only when this session could
//     have seen a builder: with another role's progress row hidden, or
//     activity tracking off, the entry is reported without an owner.
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
		indexOID   *uint32
		indexValid *bool
		indexTable *uint32
		tableName  *string
		droppable  *bool
		builderPID *int32
		hiddenRows int64
		tracking   bool
	)
	// The invalid-entry facts come from one LATERAL row so a missing entry
	// is one NULL row, not five subqueries that could each miss.
	err := q.QueryRow(ctx,
		`SELECT (SELECT c.oid
		           FROM pg_catalog.pg_class c
		           JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		          WHERE n.nspname OPERATOR(pg_catalog.=) $1
		            AND c.relname OPERATOR(pg_catalog.=) $2),
		        idx.oid, idx.indisvalid, idx.indrelid, idx.table_name, idx.droppable, idx.builder_pid,
		        `+builderVisibilityColumns+`
		   FROM (SELECT 1) AS one
		   LEFT JOIN LATERAL (
		        SELECT c.oid, i.indisvalid, i.indrelid, t.relname AS table_name,
		               `+droppableColumn+` AS droppable,
		               `+builderPIDSubquery+` AS builder_pid
		          FROM pg_catalog.pg_index i
		          JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		          JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		          JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		         WHERE n.nspname OPERATOR(pg_catalog.=) $1
		           AND c.relname OPERATOR(pg_catalog.=) $3) AS idx ON true`,
		target.schema, build.table, build.index).Scan(&currentOID, &indexOID, &indexValid, &indexTable, &tableName, &droppable, &builderPID, &hiddenRows, &tracking)
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
	if indexOID == nil || indexTable == nil || tableName == nil || droppable == nil {
		// The entry's row was read, so every one of its columns is
		// non-null by the catalog's own definition; a NULL is a catalog
		// this verdict does not understand, and indeterminate fails closed.
		return fail(fmt.Errorf("inspect index %s.%s: entry row carries NULL identity facts: %w", target.schema, build.index, ErrAbandonmentUnproven))
	}
	existing := invalidIndex{oid: *indexOID, tableOID: *indexTable, table: *tableName, droppable: *droppable,
		builder: newBuilderFacts(builderPID, hiddenRows, tracking)}
	owned := classifyInvalidIndex(existing, target, build.index, ErrBuildLeftInvalidIndex)
	owned.Build = buildErr
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
// schema scopes the index-name inspections, the table OID is the identity
// the post-failure verdict re-proves — a table swapped under the same name
// makes the verdict indeterminate rather than clean — and the table name is
// the one the statement gave, which the OID must still answer to whenever a
// later observation is compared against the resolution: a rename keeps the
// OID, and the statement no longer names the table.
type indexTarget struct {
	tableOID uint32
	schema   string
	table    string
}

// querier is the query surface the catalog helpers need; *pgxpool.Conn and
// *pgxpool.Pool both satisfy it.
//
// Every proof query these helpers run names its relations, functions, and
// operators with an explicit pg_catalog qualification (CO-9): search_path may
// legitimately list a user schema before pg_catalog, and a user relation
// named pg_index — or a user operator named = — would silently shadow the
// catalog and turn a fail-closed proof into a false clean. Only the
// admitted build statement itself uses the session's normal resolution.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// resolveTarget resolves the statement's qualified table to its OID, schema
// and name, proving it exists. Admission guarantees qualification, so the
// resolution is session-independent — the same name yields the same table
// on the build session and on the verdict's pool session alike.
func resolveTarget(ctx context.Context, q querier, build concurrentIndexBuild) (indexTarget, error) {
	ref := pgx.Identifier{build.tableSchema, build.table}
	var target indexTarget
	err := q.QueryRow(ctx,
		`SELECT c.oid, n.nspname, c.relname
		   FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)`,
		ref.Sanitize()).Scan(&target.tableOID, &target.schema, &target.table)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, fmt.Errorf("resolve table %s: %w", ref.Sanitize(), ErrTableNotFound)
	}
	if err != nil {
		return target, fmt.Errorf("resolve table %s: %w", ref.Sanitize(), err)
	}
	return target, nil
}
