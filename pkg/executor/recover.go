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

// abandonmentProofLockTimeout bounds the SHARE UPDATE EXCLUSIVE table lock
// the recovery takes to prove abandonment. The lock conflicts only with
// other schema-changing work on the table (a concurrent build in
// progress, VACUUM, another DDL), never with reads or writes; a wait that
// long means such work is running, and the recovery reports that rather
// than queueing behind it.
const abandonmentProofLockTimeout = 5 * time.Second

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

// IndexRecoveryReport says what RebuildAbandonedIndex did: which abandoned
// entries it removed, then the verified build.
type IndexRecoveryReport struct {
	// Dropped lists the abandoned invalid indexes removed before the build,
	// in removal order; empty when the table carried none.
	Dropped []DroppedIndex `json:"dropped"`
	// Build is the verified report of the requested index build.
	Build IndexBuildReport `json:"build"`
}

// RebuildAbandonedIndex removes the abandoned invalid index occupying the
// name a CREATE INDEX ... CONCURRENTLY statement asks for, then runs the
// build. It accepts exactly the statements BuildIndexConcurrently accepts,
// under the same budget and pool requirements, and is the recovery the
// build's ErrAbandonedInvalidIndex and ErrBuildLeftInvalidIndex outcomes
// name. With no invalid index under the name it is the build alone.
//
// The removal is safe because it is proven, not because it is named:
//
//  1. Under a bounded SHARE UPDATE EXCLUSIVE lock on the target table —
//     the lock every concurrent index command holds for its whole life, so
//     holding it means no build, drop, or reindex of any index on the
//     table is in flight — the entry is re-verified by OID (still under
//     the requested name, still invalid, still on this table, no builder)
//     and renamed to a name derived from that OID. A lock not granted
//     within the bound is reported as a *BudgetError (CauseLock) and
//     nothing is touched.
//  2. The rename commits, and every invalid index on the table whose name
//     is its own quarantine name is dropped with DROP INDEX CONCURRENTLY,
//     each verified by OID before and after. The quarantine name can
//     belong to nothing but the entry it was derived from, so the drop by
//     name is a drop by identity; and because the sweep keys on the name
//     pattern, a crash between rename and drop leaves debris a later
//     recovery removes on its own.
//  3. The requested build runs exactly as BuildIndexConcurrently.
//
// The recovery refuses, touching nothing, when the invalid index under the
// name is visibly another backend's build still in progress
// (ErrInvalidIndexBuildInFlight) or sits on a different table
// (ErrInvalidIndexOnOtherTable) — see (*InvalidIndexError).Recoverable —
// and fails closed with ErrAbandonmentUnproven when the entry changes
// between two verification points or a drop leaves it in place. A builder
// this role cannot observe (ErrInvalidIndexBuilderUnobservable at the
// build) is not a refusal here: the lock in step 1 is the proof, and a
// hidden build holds the lock like any other, so the recovery reports the
// lock budget instead of waiting behind it. It never removes a valid index.
func RebuildAbandonedIndex(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget) (IndexRecoveryReport, error) {
	var rep IndexRecoveryReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	// INV: LK-2 — the drops and the build run in caller-owned mode under the
	// caller's cancellation alone, so a context without one is refused
	// before any session use.
	if b.CallerOwned && ctx.Done() == nil {
		return rep, ErrCallerOwnedNeedsCancellableContext
	}
	build, err := admitConcurrentIndexBuild(sql)
	if err != nil {
		return rep, err
	}
	if pool.Config().MaxConns < 2 {
		return rep, ErrPoolTooSmall
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
		refusal := classifyInvalidIndex(existing, target, build.index)
		if !refusal.Recoverable() {
			return rep, refusal
		}
		if err := quarantineAbandonedIndex(ctx, conn, target, build.index, existing); err != nil {
			return rep, err
		}
	}

	rep.Dropped, err = dropQuarantinedIndexes(ctx, pool, conn, target, build.index, b)
	if err != nil {
		return rep, err
	}
	rep.Build, err = buildIndexConcurrently(ctx, pool, sql, b, nil)
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
		return &InvalidIndexError{Schema: target.schema, Index: index, Table: existing.table, Cleanup: cleanup}
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
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = "+strconv.FormatInt(abandonmentProofLockTimeout.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("set abandonment proof lock budget: %w", err)
	}
	table := pgx.Identifier{target.schema, existing.table}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table.Sanitize()+" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable {
			return &BudgetError{Cause: CauseLock, Budget: abandonmentProofLockTimeout}
		}
		return fmt.Errorf("lock table %s for abandonment proof: %w", table.Sanitize(), err)
	}

	// Re-verify by identity under the lock: the table OID still carries
	// the resolved name, and the index OID still carries the requested
	// name, is still invalid, still on this table, and has no builder.
	// Anything else means the catalog moved since the inspection; the
	// recovery starts over rather than act on a stale observation.
	var (
		tableName  *string
		indexName  *string
		indexValid *bool
		indexTable *uint32
		builderPID *int32
	)
	err = tx.QueryRow(ctx,
		`SELECT (SELECT t.relname FROM pg_catalog.pg_class t WHERE t.oid OPERATOR(pg_catalog.=) $1),
		        c.relname, i.indisvalid, i.indrelid, `+builderPIDSubquery+`
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		  WHERE c.oid OPERATOR(pg_catalog.=) $2`,
		target.tableOID, existing.oid).Scan(&tableName, &indexName, &indexValid, &indexTable, &builderPID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The entry is gone: someone else removed it. Nothing to
		// quarantine; the build will find the name free or refuse anew.
		return nil
	}
	if err != nil {
		return fail(fmt.Errorf("re-verify index %s.%s under lock: %w", target.schema, index, err))
	}
	if tableName == nil || *tableName != existing.table {
		return fail(ErrTargetIdentityChanged)
	}
	if indexName == nil || *indexName != index || indexValid == nil || *indexValid || indexTable == nil || *indexTable != target.tableOID {
		return fail(ErrAbandonmentUnproven)
	}
	if pid := pidValue(builderPID); pid != 0 {
		return &InvalidIndexError{Schema: target.schema, Index: index, Table: existing.table, BuilderPID: pid, Cleanup: ErrInvalidIndexBuildInFlight}
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

// dropQuarantinedIndexes removes every invalid index on the target table
// that carries its own quarantine name, one DROP INDEX CONCURRENTLY each on
// a budgeted session, each verified by OID before and after. The sweep is
// what makes the recovery restartable: an entry quarantined by a run that
// died before its drop is removed by the next run.
func dropQuarantinedIndexes(ctx context.Context, pool *pgxpool.Pool, q querier, target indexTarget, index string, b ConcurrentBudget) ([]DroppedIndex, error) {
	quarantined, err := listQuarantinedIndexes(ctx, q, target)
	if err != nil {
		return nil, err
	}
	var dropped []DroppedIndex
	for _, entry := range quarantined {
		if entry.builder.pid != 0 {
			// Someone is visibly rebuilding a quarantined entry; it is
			// not abandoned while they are, so the recovery stops here. A
			// builder this session cannot see holds the table lock the
			// drop needs, and surfaces as the drop's lock budget instead.
			return dropped, &InvalidIndexError{Schema: target.schema, Index: index, Table: entry.table,
				BuilderPID: entry.builder.pid, Cleanup: ErrInvalidIndexBuildInFlight}
		}
		d, err := dropQuarantinedIndex(ctx, pool, target, index, entry, b)
		if err != nil {
			return dropped, err
		}
		dropped = append(dropped, d)
	}
	return dropped, nil
}

// listQuarantinedIndexes reads the invalid indexes on the target table
// whose name is exactly their own quarantine name. The equality is against
// the name derived from each row's OID, so an operator-created index that
// happens to start with the prefix is never matched.
func listQuarantinedIndexes(ctx context.Context, q querier, target indexTarget) ([]invalidIndex, error) {
	rows, err := q.Query(ctx,
		`SELECT c.oid, i.indrelid, t.relname, `+builderPIDSubquery+`, `+builderVisibilityColumns+`
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
		if err := rows.Scan(&entry.oid, &entry.tableOID, &entry.table, &builderPID, &hiddenRows, &tracking); err != nil {
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

// dropQuarantinedIndex drops one quarantined entry on its own budgeted
// session — DROP INDEX CONCURRENTLY waits for other transactions' snapshots
// the way the build does, so it runs under the same wait policy — and
// verifies by OID that the name still identifies the entry before the drop
// and that the entry is gone after it.
func dropQuarantinedIndex(ctx context.Context, pool *pgxpool.Pool, target indexTarget, index string, entry invalidIndex, b ConcurrentBudget) (DroppedIndex, error) {
	name := quarantineName(entry.oid)
	fail := func(cleanup error) error {
		return &InvalidIndexError{Schema: target.schema, Index: index, Table: entry.table,
			Cleanup: fmt.Errorf("quarantined index %s.%s: %w", target.schema, name, cleanup)}
	}
	conn, release, err := acquireBudgetedSession(ctx, pool, b)
	if err != nil {
		return DroppedIndex{}, err
	}
	defer release()

	// The name must resolve to the OID it was derived from, still invalid,
	// still on this table. Any other answer means the catalog moved.
	var (
		oid   uint32
		valid bool
		table uint32
	)
	err = conn.QueryRow(ctx,
		`SELECT c.oid, i.indisvalid, i.indrelid
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
		target.schema, name).Scan(&oid, &valid, &table)
	if err != nil {
		return DroppedIndex{}, fail(fmt.Errorf("re-verify before drop: %w", err))
	}
	if oid != entry.oid || valid || table != target.tableOID {
		return DroppedIndex{}, fail(ErrAbandonmentUnproven)
	}

	start := time.Now()
	_, dropErr := conn.Exec(ctx, "DROP INDEX CONCURRENTLY "+pgx.Identifier{target.schema, name}.Sanitize())
	elapsed := time.Since(start)
	if dropErr != nil {
		return DroppedIndex{}, asConcurrentBudgetError(dropErr, b, elapsed, "drop quarantined index "+pgx.Identifier{target.schema, name}.Sanitize())
	}
	// A reported success is trusted only once the OID is gone: DROP INDEX
	// CONCURRENTLY that fails midway leaves the entry in place, marked
	// invalid, which it already was.
	var remaining int
	if err := conn.QueryRow(ctx,
		`SELECT pg_catalog.count(*) FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) $1`,
		entry.oid).Scan(&remaining); err != nil {
		return DroppedIndex{}, fail(fmt.Errorf("verify drop: %w", err))
	}
	if remaining != 0 {
		return DroppedIndex{}, fail(ErrAbandonmentUnproven)
	}
	return DroppedIndex{Schema: target.schema, Index: name, IndexOID: entry.oid, Duration: elapsed}, nil
}
