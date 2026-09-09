// This file is the recovery for an abandoned concurrent index build: the
// automatic counterpart of the refusal in native.go. A failed CREATE INDEX
// CONCURRENTLY leaves an invalid catalog entry that occupies the requested
// name, and PostgreSQL drops by name, not identity — so removing it needs a
// proof that the entry under that name is the abandoned one and that
// nobody is building it, held all the way to the drop. The proof here is a
// lock: every concurrent index command (CREATE, DROP, REINDEX ...
// CONCURRENTLY) holds SHARE UPDATE EXCLUSIVE on the table for its whole
// life, so taking that lock ourselves excludes them all, and renaming the
// entry by identity to a name derived from its OID gives the later drop a
// name that can belong to nothing else.

package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recoveryLockTimeout bounds every lock wait the recovery makes on its own
// behalf: the SHARE UPDATE EXCLUSIVE table lock of the abandonment proof,
// and the waits inside each DROP INDEX CONCURRENTLY — for that same table
// lock, and for the transactions that can still see the entry. Both
// conflict only with other schema-changing work on the table or with
// long-running transactions, never with ordinary reads or writes; a wait
// that long means such work is running, and the recovery reports it as a
// lock budget rather than queueing behind it. The drop can take a per-lock
// bound where the build cannot: a build cancelled mid-wait leaves a new
// invalid index behind, while a drop cancelled mid-wait leaves the entry
// exactly as it was — quarantined, invalid, and swept by the next run.
const recoveryLockTimeout = 5 * time.Second

// sqlstateUndefinedTable is the SQLSTATE the server raises when a
// statement names a table that does not exist.
const sqlstateUndefinedTable = "42P01"

// quarantinePrefix is the name prefix of an invalid index the recovery has
// proven abandoned and renamed by identity; the suffix is the entry's own
// OID, so the full name identifies exactly one catalog entry and the drop
// that follows can be verified against it.
const quarantinePrefix = "pgsprite_abandoned_"

// quarantineName is the identity-derived name an abandoned entry is
// renamed to before it is dropped.
func quarantineName(oid uint32) string {
	return quarantinePrefix + strconv.FormatUint(uint64(oid), 10)
}

// DroppedIndex records one abandoned invalid index the recovery removed.
type DroppedIndex struct {
	// Schema is the schema the entry lived in.
	Schema string `json:"schema"`
	// Index is the entry's name at drop time — its identity-derived
	// quarantine name, not the name the failed build gave it.
	Index string `json:"index"`
	// IndexOID is the removed entry's catalog identity.
	IndexOID uint32 `json:"index_oid"`
	// Duration is the wall-clock time of the DROP INDEX CONCURRENTLY
	// statement itself. It encodes as integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
}

// QuarantinedIndex records one quarantined entry the recovery found on the
// table and deliberately left in place.
type QuarantinedIndex struct {
	// Schema is the schema the entry lives in.
	Schema string `json:"schema"`
	// Index is the entry's identity-derived quarantine name.
	Index string `json:"index"`
	// IndexOID is the entry's catalog identity.
	IndexOID uint32 `json:"index_oid"`
}

// IndexRecoveryReport says what an abandoned-index recovery did: which
// abandoned entries it removed, which quarantined entries it left alone,
// and, for RebuildAbandonedIndex, the verified build.
type IndexRecoveryReport struct {
	// Dropped lists the abandoned invalid indexes removed before the build,
	// in removal order; empty when the table carried none.
	Dropped []DroppedIndex `json:"dropped"`
	// Skipped lists the quarantined entries on the table that DROP INDEX
	// CONCURRENTLY cannot remove — an entry that became a partitioned
	// index's partition or a constraint's index after it was quarantined —
	// which the sweep leaves in place for an operator and steps over, so
	// one such entry never blocks every later recovery on the table. Empty
	// when there were none.
	Skipped []QuarantinedIndex `json:"skipped"`
	// Build is the verified report of the requested index build. It is zero
	// when returned by DropAbandonedIndex, which does not run a build.
	Build IndexBuildReport `json:"build"`
	// Duration is the wall-clock time of the whole recovery call — proof
	// and drops, plus the build when requested — so a caller that sized
	// the budget as a lease can see what the recovery actually spent. It
	// is set on every return, a refusal included, so a refusal that waited
	// out a lock bound reports the wait. It encodes as integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
}

// RebuildAbandonedIndex removes the abandoned invalid index occupying the
// name a CREATE INDEX ... CONCURRENTLY statement asks for, then runs the
// build. It accepts exactly the statements BuildIndexConcurrently accepts
// under the same budget, needs a pool one connection larger (its own
// session stays open across the drops and the build), and is the recovery
// the build's ErrAbandonedInvalidIndex, ErrBuildLeftInvalidIndex, and
// ErrInvalidIndexBuilderUnobservable outcomes name. With no invalid index
// under the name and no quarantined debris on the table it is the build
// alone.
//
// The removal is safe because it is proven, not because it is named:
//
//  1. Under a bounded SHARE UPDATE EXCLUSIVE lock on the target table —
//     the lock every concurrent index command holds for its whole life, so
//     holding it means no build, drop, or reindex of any index on the
//     table is in flight — the entry is re-verified by OID (still under
//     the requested name, still invalid, still on this table in this
//     schema, still an index the server will drop concurrently, no
//     builder) and renamed to a name derived from that OID. A lock not
//     granted within the bound is reported as a *BudgetError (CauseLock)
//     and nothing is touched.
//  2. The rename commits, and every invalid index on the table whose name
//     is its own quarantine name is dropped with DROP INDEX CONCURRENTLY,
//     each verified by OID before and after. The quarantine name can
//     belong to nothing but the entry it was derived from, so the drop by
//     name is a drop by identity; and because the sweep keys on the name
//     pattern, a crash between rename and drop leaves debris a later
//     recovery removes on its own — and a plain BuildIndexConcurrently
//     on the table refuses until it does, so the debris stays loud.
//  3. The requested build runs exactly as BuildIndexConcurrently.
//
// The recovery refuses, touching nothing, when the invalid index under the
// name is visibly another backend's build still in progress
// (ErrInvalidIndexBuildInFlight), sits on a different table
// (ErrInvalidIndexOnOtherTable), or is not an index the server will drop
// concurrently (ErrInvalidIndexNotDroppable: a partitioned table's index,
// an index partition, a constraint's index — invalid for reasons of their
// own, never a failed build's debris) — see
// (*InvalidIndexError).Recoverable — and fails closed with
// ErrAbandonmentUnproven when the entry changes between two verification
// points or a drop leaves it in place. A builder this role cannot observe
// (ErrInvalidIndexBuilderUnobservable at the build) is not a refusal here:
// the lock in step 1 is the proof, and a hidden build holds the lock like
// any other, so the recovery reports the lock budget instead of waiting
// behind it. It never removes a valid index.
//
// Time is bounded by construction. Every lock the recovery waits for on
// its own behalf — the proof lock and the drops' waits — is bounded by
// recoveryLockTimeout; the whole sweep of drops shares one Overall budget
// (each drop gets what the earlier ones left), and the build gets Overall
// once more, so the recovery takes at most the lock bound plus twice the
// budget plus the build's own bounded verdict. In caller-owned mode the
// caller's cancellation bounds the drops and the build alike. The report's
// Duration says what was actually spent.
func RebuildAbandonedIndex(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget) (IndexRecoveryReport, error) {
	start := time.Now()
	rep, err := recoverAbandonedIndex(ctx, pool, sql, b, recoveryPool{
		minConns: recoveryMinConns,
		sessions: "recovery session, build session, and reserved verdict session",
	}, func(rep *IndexRecoveryReport) error {
		var buildErr error
		rep.Build, buildErr = buildIndexConcurrently(ctx, pool, sql, b, nil)
		return buildErr
	})
	rep.Duration = time.Since(start)
	return rep, err
}

// DropAbandonedIndex proves that the invalid index occupying the name in a
// CREATE INDEX ... CONCURRENTLY statement is abandoned, quarantines it by
// OID, and drops it concurrently without running the requested build. It has
// the same admission rules, fail-closed invalid-index verdicts, and proof and
// drop budgets as RebuildAbandonedIndex. It needs two pool connections: the
// recovery session and a drop session. Build in the returned report is always
// zero.
func DropAbandonedIndex(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget) (IndexRecoveryReport, error) {
	start := time.Now()
	rep, err := recoverAbandonedIndex(ctx, pool, sql, b, recoveryPool{
		minConns: dropRecoveryMinConns,
		sessions: "recovery session and drop session",
	}, nil)
	rep.Duration = time.Since(start)
	return rep, err
}

// recoveryPool is what a recovery entry point needs of the pool at
// admission: the connections it holds at its peak, and the sessions those
// connections are, named in the refusal so an operator sizing the pool can
// tell which operation asked and why.
type recoveryPool struct {
	minConns int32
	sessions string
}

// recoverAbandonedIndex admits a concurrent build statement, proves and
// quarantines an abandoned occupant of its name, and sweeps the target
// table's identity-named quarantine debris. When after is non-nil, it runs
// while the recovery session remains held.
func recoverAbandonedIndex(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget, need recoveryPool, after func(*IndexRecoveryReport) error) (IndexRecoveryReport, error) {
	var rep IndexRecoveryReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	// INV: LK-2 — the drops run in caller-owned mode under the caller's
	// cancellation alone, so a context without one is refused before any
	// session use.
	if b.CallerOwned && ctx.Done() == nil {
		return rep, ErrCallerOwnedNeedsCancellableContext
	}
	build, err := admitConcurrentIndexBuild(sql)
	if err != nil {
		return rep, err
	}
	// INV: LK-2 — the recovery session stays open while another session
	// drops an entry, and a rebuild subsequently needs two sessions of its
	// own. A smaller pool would wait on itself instead of failing.
	if pool.Config().MaxConns < need.minConns {
		return rep, fmt.Errorf("index recovery needs %d connections (%s), pool holds %d: %w",
			need.minConns, need.sessions, pool.Config().MaxConns, ErrPoolTooSmall)
	}

	// The inspection and the proof run on a pool session under its
	// baseline budgets; the transaction narrows lock_timeout itself.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return rep, fmt.Errorf("acquire recovery session: %w", err)
	}
	defer conn.Release()

	target, err := resolveTarget(ctx, conn, build)
	if err != nil {
		return rep, err
	}
	existing, found, err := inspectInvalidIndex(ctx, conn, target.schema, build.index)
	if err != nil {
		return rep, err
	}
	if found {
		refusal := classifyInvalidIndex(existing, target, build.index, ErrAbandonedInvalidIndex)
		if !refusal.Recoverable() {
			return rep, refusal
		}
		if err := quarantineAbandonedIndex(ctx, conn, target, build.index, existing); err != nil {
			return rep, err
		}
	}

	rep.Dropped, rep.Skipped, err = dropQuarantinedIndexes(ctx, pool, conn, target, b)
	if err != nil {
		return rep, err
	}
	if after != nil {
		err = after(&rep)
	}
	return rep, err
}

// quarantineAbandonedIndex proves the observed invalid index abandoned and
// renames it to its quarantine name, in one transaction under a bounded
// SHARE UPDATE EXCLUSIVE lock on the target table. The proof and the
// rename share the transaction, so no concurrent index command on the
// table can start between them; the re-verification inside the lock
// closes the window between the caller's inspection and the lock grant.
func quarantineAbandonedIndex(ctx context.Context, conn *pgxpool.Conn, target indexTarget, index string, existing invalidIndex) error {
	fail := func(cleanup error) error {
		return &InvalidIndexError{Schema: target.schema, Index: index, Table: target.table, Cleanup: cleanup}
	}
	// The observation must be of the table the statement names. The entry's
	// table OID already matched the resolution, but a rename between the
	// resolution and the inspection keeps the OID and changes the name:
	// the inspection then reports the table under a name the statement
	// never gave, and locking that name would follow the rename silently.
	if existing.table != target.table {
		return fail(ErrTargetIdentityChanged)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin abandonment proof: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path a rollback error
		// only means the connection died, and the server aborts the
		// transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	// INV: LK-2 — the proof lock is bounded inside its own transaction
	// regardless of the pool's defaults. A bare integer is milliseconds to
	// PostgreSQL.
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = "+strconv.FormatInt(recoveryLockTimeout.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("set abandonment proof lock budget: %w", err)
	}
	// LOCK TABLE takes a name, not an identity: a table renamed or dropped
	// since the inspection is no longer the table the statement names and
	// fails here as undefined, and a table replaced under the same name is
	// caught by the identity re-check below, once the lock on whatever now
	// bears the name is held.
	table := pgx.Identifier{target.schema, target.table}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table.Sanitize()+" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable:
			return &BudgetError{Cause: CauseLock, Budget: recoveryLockTimeout}
		case errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedTable:
			return fail(ErrTargetIdentityChanged)
		}
		return fmt.Errorf("lock table %s for abandonment proof: %w", table.Sanitize(), err)
	}

	// Re-verify by identity under the lock: the table OID still carries
	// the resolved name in the resolved schema, and the index OID is still
	// an abandonment candidate under the requested name. Anything else
	// means the catalog moved since the inspection; the recovery starts
	// over rather than act on a stale observation. The table's schema is
	// part of its identity: a table moved out of the schema with a
	// same-named table left behind still answers to its OID, but is no
	// longer the table the statement names. An index always lives in its
	// table's schema, so the table's schema is the index's too.
	// INV: LK-5 — the proof and the rename share this lock and this
	// transaction; nothing between them can start a concurrent index
	// command on the table.
	var (
		tableName   *string
		tableSchema *string
		facts       indexFacts
		builderPID  *int32
	)
	err = tx.QueryRow(ctx,
		`SELECT t.relname, tn.nspname, c.oid, c.relname, i.indisvalid, i.indrelid, `+droppableColumn+`, `+builderPIDSubquery+`
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   LEFT JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) $1
		   LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid OPERATOR(pg_catalog.=) t.relnamespace
		  WHERE c.oid OPERATOR(pg_catalog.=) $2`,
		target.tableOID, existing.oid).Scan(&tableName, &tableSchema, &facts.oid, &facts.name, &facts.valid, &facts.tableOID, &facts.droppable, &builderPID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The entry is gone: someone else removed it. Nothing to
		// quarantine; the build will find the name free or refuse anew.
		return nil
	}
	if err != nil {
		return fail(fmt.Errorf("re-verify index %s.%s under lock: %w", target.schema, index, err))
	}
	if tableName == nil || *tableName != target.table || tableSchema == nil || *tableSchema != target.schema {
		return fail(ErrTargetIdentityChanged)
	}
	if !facts.isAbandonmentCandidate(existing.oid, index, target) {
		return fail(ErrAbandonmentUnproven)
	}
	if pid := pidValue(builderPID); pid != 0 {
		return &InvalidIndexError{Schema: target.schema, Index: index, Table: target.table, BuilderPID: pid, Cleanup: ErrInvalidIndexBuildInFlight}
	}

	quarantined := quarantineName(existing.oid)
	if _, err := tx.Exec(ctx, "ALTER INDEX "+pgx.Identifier{target.schema, index}.Sanitize()+" RENAME TO "+pgx.Identifier{quarantined}.Sanitize()); err != nil {
		return fail(fmt.Errorf("quarantine index %s.%s: %w", target.schema, index, err))
	}
	// The rename is verified by identity before it commits: the OID the
	// proof keyed on must now carry the quarantine name, or the statement
	// renamed something the proof never examined.
	var renamed string
	if err := tx.QueryRow(ctx,
		`SELECT c.relname FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) $1`,
		existing.oid).Scan(&renamed); err != nil {
		return fail(fmt.Errorf("verify quarantine of index %s.%s: %w", target.schema, index, err))
	}
	if renamed != quarantined {
		return fail(ErrAbandonmentUnproven)
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(fmt.Errorf("commit quarantine of index %s.%s: %w", target.schema, index, err))
	}
	return nil
}

// indexFacts is one re-read of an index entry by the catalog: the facts an
// abandonment proof compares with its earlier observation before it acts.
type indexFacts struct {
	oid       uint32
	name      string
	valid     bool
	tableOID  uint32
	droppable bool
}

// isAbandonmentCandidate reports whether the entry is still what the proof
// observed and may act on: the expected identity under the expected name,
// invalid, on the target table, and an index the server will drop
// concurrently.
func (f indexFacts) isAbandonmentCandidate(oid uint32, name string, target indexTarget) bool {
	return f.oid == oid && f.name == name && !f.valid && f.tableOID == target.tableOID && f.droppable
}

// dropQuarantinedIndexes removes every invalid index on the target table
// that carries its own quarantine name and that DROP INDEX CONCURRENTLY can
// remove — one statement each on a budgeted session, each verified by OID
// before and after — and reports the quarantined entries it leaves in
// place. The sweep is what makes the recovery restartable: an entry
// quarantined by a run that died before its drop is removed by the next
// run. The drops share one budget: each gets what the earlier ones left,
// so a table with many entries is bounded like a table with one.
func dropQuarantinedIndexes(ctx context.Context, pool *pgxpool.Pool, q querier, target indexTarget, b ConcurrentBudget) ([]DroppedIndex, []QuarantinedIndex, error) {
	quarantined, err := listQuarantinedIndexes(ctx, q, target)
	if err != nil {
		return nil, nil, err
	}
	var (
		dropped []DroppedIndex
		skipped []QuarantinedIndex
	)
	sweepStart := time.Now()
	for _, entry := range quarantined {
		name := quarantineName(entry.oid)
		if !entry.droppable {
			// The entry became a partitioned index's partition or a
			// constraint's index after it was quarantined; the server
			// refuses to drop it concurrently. It is an operator's to
			// resolve, and the sweep steps over it so it never blocks
			// every later recovery on the table.
			skipped = append(skipped, QuarantinedIndex{Schema: target.schema, Index: name, IndexOID: entry.oid})
			continue
		}
		if entry.builder.pid != 0 {
			// Someone is visibly rebuilding a quarantined entry; it is
			// not abandoned while they are, so the recovery stops here. A
			// builder this session cannot see holds the table lock the
			// drop needs, and surfaces as the drop's lock budget.
			return dropped, skipped, &InvalidIndexError{Schema: target.schema, Index: name, Table: entry.table,
				BuilderPID: entry.builder.pid, Cleanup: ErrInvalidIndexBuildInFlight}
		}
		remaining, err := b.remainingAfter(time.Since(sweepStart))
		if err != nil {
			return dropped, skipped, err
		}
		d, err := dropQuarantinedIndex(ctx, pool, target, entry, remaining)
		if err != nil {
			return dropped, skipped, err
		}
		dropped = append(dropped, d)
	}
	return dropped, skipped, nil
}

// remainingAfter is the budget left once spent of it has gone to earlier
// work under the same budget. A caller-owned budget is the caller's
// cancellation and passes through unchanged. A served budget with less
// than the server's one-millisecond granularity left would round to a
// disabled statement_timeout, so an exhausted budget is reported as spent
// rather than handed on unbounded.
func (b ConcurrentBudget) remainingAfter(spent time.Duration) (ConcurrentBudget, error) {
	if b.CallerOwned {
		return b, nil
	}
	left := b.Overall - spent
	if left < time.Millisecond {
		return ConcurrentBudget{}, &BudgetError{Cause: CauseStatement, Budget: b.Overall}
	}
	return ConcurrentBudget{Overall: left}, nil
}

// listQuarantinedIndexes reads the invalid indexes on the target table
// whose name is exactly their own quarantine name. The equality is against
// the name derived from each row's OID, so an operator-created index that
// happens to start with the prefix is never matched.
func listQuarantinedIndexes(ctx context.Context, q querier, target indexTarget) ([]invalidIndex, error) {
	rows, err := q.Query(ctx,
		`SELECT c.oid, i.indrelid, t.relname, `+droppableColumn+`, `+builderPIDSubquery+`, `+builderVisibilityColumns+`
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		  WHERE i.indrelid OPERATOR(pg_catalog.=) $1
		    AND NOT i.indisvalid
		    AND c.relname OPERATOR(pg_catalog.=) ($2 OPERATOR(pg_catalog.||) c.oid::pg_catalog.text)
		  ORDER BY c.oid`,
		target.tableOID, quarantinePrefix)
	if err != nil {
		return nil, fmt.Errorf("list quarantined indexes on table %d: %w", target.tableOID, err)
	}
	defer rows.Close()
	var found []invalidIndex
	for rows.Next() {
		var (
			entry      invalidIndex
			builderPID *int32
			hiddenRows int64
			tracking   bool
		)
		if err := rows.Scan(&entry.oid, &entry.tableOID, &entry.table, &entry.droppable, &builderPID, &hiddenRows, &tracking); err != nil {
			return nil, fmt.Errorf("list quarantined indexes on table %d: %w", target.tableOID, err)
		}
		entry.builder = newBuilderFacts(builderPID, hiddenRows, tracking)
		found = append(found, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list quarantined indexes on table %d: %w", target.tableOID, err)
	}
	return found, nil
}

// listDroppableQuarantinedIndexes is listQuarantinedIndexes without the
// entries DROP INDEX CONCURRENTLY cannot remove: the debris a recovery
// would actually sweep, and so the debris a plain build refuses over.
func listDroppableQuarantinedIndexes(ctx context.Context, q querier, target indexTarget) ([]invalidIndex, error) {
	quarantined, err := listQuarantinedIndexes(ctx, q, target)
	if err != nil {
		return nil, err
	}
	var droppable []invalidIndex
	for _, entry := range quarantined {
		if entry.droppable {
			droppable = append(droppable, entry)
		}
	}
	return droppable, nil
}

// dropQuarantinedIndex drops one quarantined entry on its own budgeted
// session and verifies by OID that the name still identifies the entry
// before the drop and that the entry is gone after it. DROP INDEX
// CONCURRENTLY waits for the transactions that can still see the entry the
// way the build does, so it runs under the same overall budget — but with a
// per-lock bound the build cannot afford, because a drop cancelled
// mid-wait leaves the entry exactly as it was for the next sweep.
func dropQuarantinedIndex(ctx context.Context, pool *pgxpool.Pool, target indexTarget, entry invalidIndex, b ConcurrentBudget) (DroppedIndex, error) {
	name := quarantineName(entry.oid)
	ref := pgx.Identifier{target.schema, name}
	fail := func(cleanup error) error {
		return &InvalidIndexError{Schema: target.schema, Index: name, Table: entry.table, Cleanup: cleanup}
	}
	conn, release, err := acquireBudgetedSession(ctx, pool, b)
	if err != nil {
		return DroppedIndex{}, err
	}
	defer release()
	// INV: LK-2 — the drop's lock waits are bounded on top of the
	// CONCURRENTLY wait policy the session came with; the release resets
	// the setting with the rest. A bare integer is milliseconds to
	// PostgreSQL.
	if _, err := conn.Exec(ctx, "SET lock_timeout = "+strconv.FormatInt(recoveryLockTimeout.Milliseconds(), 10)); err != nil {
		return DroppedIndex{}, fmt.Errorf("set drop lock budget: %w", err)
	}

	// INV: LK-5 — the name must resolve to the OID it was derived from,
	// still an abandonment candidate. Any other answer means the catalog
	// moved, and a drop by name would hit an entry the proof never covered.
	var facts indexFacts
	err = conn.QueryRow(ctx,
		`SELECT c.oid, c.relname, i.indisvalid, i.indrelid, `+droppableColumn+`
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
		target.schema, name).Scan(&facts.oid, &facts.name, &facts.valid, &facts.tableOID, &facts.droppable)
	if errors.Is(err, pgx.ErrNoRows) {
		// The sweep listed the entry moments ago; a name that resolves to
		// nothing now means someone else is acting on the table's indexes.
		return DroppedIndex{}, fail(ErrAbandonmentUnproven)
	}
	if err != nil {
		return DroppedIndex{}, fail(fmt.Errorf("re-verify %s before drop: %w", ref.Sanitize(), err))
	}
	if !facts.isAbandonmentCandidate(entry.oid, name, target) {
		return DroppedIndex{}, fail(ErrAbandonmentUnproven)
	}

	start := time.Now()
	_, dropErr := conn.Exec(ctx, "DROP INDEX CONCURRENTLY "+ref.Sanitize())
	elapsed := time.Since(start)
	if dropErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(dropErr, &pgErr) && pgErr.Code == sqlstateLockNotAvailable {
			return DroppedIndex{}, &BudgetError{Cause: CauseLock, Budget: recoveryLockTimeout}
		}
		outcome := asConcurrentBudgetError(ctx, dropErr, b, elapsed, "drop quarantined index "+ref.Sanitize())
		if isBoundedOutcome(outcome) {
			return DroppedIndex{}, outcome
		}
		return DroppedIndex{}, fail(outcome)
	}
	// A reported success is trusted only once the OID is gone: DROP INDEX
	// CONCURRENTLY that fails midway leaves the entry in place, marked
	// invalid, which it already was.
	var remaining int
	if err := conn.QueryRow(ctx,
		`SELECT pg_catalog.count(*) FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) $1`,
		entry.oid).Scan(&remaining); err != nil {
		return DroppedIndex{}, fail(fmt.Errorf("verify drop of %s: %w", ref.Sanitize(), err))
	}
	if remaining != 0 {
		return DroppedIndex{}, fail(ErrAbandonmentUnproven)
	}
	return DroppedIndex{Schema: target.schema, Index: name, IndexOID: entry.oid, Duration: elapsed}, nil
}

// isBoundedOutcome reports whether a statement's failure is one of the
// time-bound outcomes — a budget exhausted, the caller's own cancellation,
// or an external one — that callers branch on directly and that must
// therefore not be wrapped in a verdict about the entry. A drop cancelled
// mid-wait leaves the entry exactly as it was for the next sweep; wrapping
// the cancellation would report that known state as unproven.
func isBoundedOutcome(err error) bool {
	var budgetErr *BudgetError
	return errors.As(err, &budgetErr) ||
		errors.Is(err, ErrCancelledByCaller) ||
		errors.Is(err, ErrCancelledExternally)
}
