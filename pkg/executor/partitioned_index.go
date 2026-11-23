// This file is the partitioned index build: the online way to add an index to
// a partitioned table. PostgreSQL cannot build one concurrently, and a plain
// CREATE INDEX on a partitioned table blocks writes to the whole tree. The
// documented alternative is three steps: create the parent's index with ONLY,
// which builds nothing; build each partition's index concurrently; attach each
// one. This executor runs them partition by partition under the parent's table
// lock, resumable at every step (docs/partitioned-index-build-design.md). A
// partition's build is the concurrent build executor's own (cic_build.go), with
// the same budget, failure verdict, and debris proofs as an index on a plain
// table.

package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// maxPartitionedIndexPasses bounds how many times the build walks the
// partition tree. The first pass does the work and a later one confirms there
// is nothing left; the rest are the room other sessions' partition changes get
// before the build refuses instead of chasing them for ever.
const maxPartitionedIndexPasses = 5

// PartitionIndexAction says what the build did to give one partition an index
// attached to the parent's.
type PartitionIndexAction string

const (
	// PartitionIndexBuilt means the build created the partition's index
	// concurrently, then attached it.
	PartitionIndexBuilt PartitionIndexAction = "built"
	// PartitionIndexReused means the partition already had a valid index of
	// the requested definition, under the derived name, that no partitioned
	// index owned: one an earlier run built and did not attach.
	PartitionIndexReused PartitionIndexAction = "reused"
	// PartitionIndexAdopted means the partition already had a valid index of
	// the requested definition under another name, and the build attached it
	// instead of building one. From then on it is a partition of the parent's
	// index, which the server refuses to drop on its own.
	PartitionIndexAdopted PartitionIndexAction = "adopted"
	// PartitionIndexOnly means the partition is itself partitioned, so the
	// build gave it a metadata-only index, filled by its partitions' indexes,
	// and attached that.
	PartitionIndexOnly PartitionIndexAction = "partitioned"
)

// PartitionIndexResult is one partition the build had work to do on. A
// partition that already had its index attached is counted in
// PartitionedIndexReport.Partitions and not listed here.
type PartitionIndexResult struct {
	// Schema and Table identify the partition.
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// Index is the name of the partition's index that is now attached.
	Index string `json:"index"`
	// IndexOID is that index's catalog identity.
	IndexOID uint32 `json:"index_oid"`
	// Action says how the partition got its index.
	Action PartitionIndexAction `json:"action"`
	// RecoveredDebris is true when the build first removed the invalid
	// leftover of an earlier failed build of this partition's index, under the
	// proof RebuildAbandonedIndex gives.
	RecoveredDebris bool `json:"recovered_debris,omitempty"`
}

// PartitionedIndexReport says what a partitioned index build did. It is
// returned only after the executor re-read the parent's index and verified it
// valid, so it means "verified valid, every partition attached".
type PartitionedIndexReport struct {
	// Schema and Index identify the parent's index.
	Schema string `json:"schema"`
	Index  string `json:"index"`
	// IndexOID is the verified index's catalog identity.
	IndexOID uint32 `json:"index_oid"`
	// ParentCreated is true when this run created the parent's index, and
	// false when it resumed one an earlier run left invalid.
	ParentCreated bool `json:"parent_created"`
	// Partitions is the number of partitions in the tree, at every depth.
	Partitions int `json:"partitions"`
	// Worked lists the partitions this run gave an index, in order.
	Worked []PartitionIndexResult `json:"worked,omitempty"`
	// Passes is how many times the build walked the tree.
	Passes int `json:"passes"`
	// Duration is the wall-clock time of the whole build. It encodes as
	// integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
	// ServerVersion is the server_version of the server that ran the build.
	ServerVersion string `json:"server_version"`
}

// BuildPartitionedIndex adds the index a CREATE INDEX statement describes to a
// partitioned table, online, and returns once the parent's index is valid and
// every partition in the tree has an index attached to it.
//
// The statement is one named, schema-qualified CREATE INDEX on the preflighted
// partitioned table, with or without CONCURRENTLY and ON ONLY; every form asks
// for a complete index on the whole tree. The build holds the parent's advisory
// table lock (LK-1) for its whole run, so a second run on the same parent is
// refused with a *dbconn.TableLockHeldError; cfg is the connection target that
// lock's dedicated session dials, the same Config the pool was made from. Under
// the lock it:
//
//  1. refuses a tree that holds a foreign table (ErrForeignTablePartition),
//     and a unique index that does not contain the partition key of every
//     partitioned table in the tree (ErrUniqueIndexPartitionKey);
//  2. creates the parent's index with ON ONLY, or resumes one an earlier run
//     left when it is a partitioned index of the requested definition
//     (ErrPartitionedIndexNameTaken otherwise);
//  3. gives each partition an attached index, recursively: a leaf reuses or
//     adopts a matching valid index or builds one concurrently under the
//     derived name, and a sub-partitioned partition gets its own ON ONLY index,
//     filled the same way, before it is attached;
//  4. walks the tree again, up to a fixed number of passes, for partitions
//     created or attached meanwhile (ErrPartitionedIndexUnsettled when they keep
//     appearing);
//  5. re-reads the parent's index and returns only when it is valid.
//
// A failure at any point leaves a valid prefix, and rerunning the same
// statement resumes from it. An index adopted from a partition is attached, not
// copied: it is removed with the parent's index or by detaching its partition.
// The build never drops an index it did not prove abandoned.
//
// Every partition's build is bounded by b, one budget per partition; every
// other statement runs under the pool's own bounded lock and statement
// timeouts. The pool must come from pkg/dbconn and hold at least two
// connections, the concurrent build's own minimum. Partitions are listed from
// the catalog at run time, not from a plan, because they can change between
// planning and running.
func BuildPartitionedIndex(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ConcurrentBudget) (PartitionedIndexReport, error) {
	return buildPartitionedIndex(ctx, cfg, pool, pt, sql, b, nil)
}

// BuildPartitionedIndexWithProgress runs a partitioned index build while
// updating tracker, which the caller may poll concurrently. A step is one
// partition, the parent's index being step one, and total_steps is the
// partitions found plus one, growing when the build finds more. It is an upper
// bound: a partition that already has its index attached takes no step, so a
// resumed run finishes below it.
func BuildPartitionedIndexWithProgress(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ConcurrentBudget, tracker *progress.Tracker) (rep PartitionedIndexReport, err error) {
	if tracker == nil {
		return rep, fmt.Errorf("%w: progress tracker is required", ErrInvariantViolation)
	}
	tracker.Start(1, progress.OperationBrief)
	defer func() { tracker.Finish(err) }()
	return buildPartitionedIndex(ctx, cfg, pool, pt, sql, b, tracker)
}

func buildPartitionedIndex(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ConcurrentBudget, tracker *progress.Tracker) (PartitionedIndexReport, error) {
	var rep PartitionedIndexReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	// INV: LK-2 — caller-owned mode is bounded by the caller's cancellation
	// signal, so a context without one is refused before any session use.
	if b.CallerOwned && ctx.Done() == nil {
		return rep, ErrCallerOwnedNeedsCancellableContext
	}
	req, err := admitPartitionedIndex(sql, pt)
	if err != nil {
		return rep, err
	}
	// INV: LK-2 — a partition's build needs its session and the reserved
	// verdict session at once; a smaller pool would make every failed build
	// indeterminate or wait on itself.
	if pool.Config().MaxConns < buildMinConns {
		return rep, fmt.Errorf("partitioned index build needs %d connections (build session and reserved verdict session), pool holds %d: %w",
			buildMinConns, pool.Config().MaxConns, ErrPoolTooSmall)
	}

	start := time.Now()
	if tracker != nil {
		start = tracker.Now()
	}
	// INV: LK-1 — the parent's advisory lock excludes a second run on the same
	// parent for the whole build.
	lock, err := dbconn.AcquireTableLock(ctx, cfg, req.schema, req.table)
	if err != nil {
		return rep, fmt.Errorf("lock %s for the partitioned index build: %w", qualifiedName(req.schema, req.table), err)
	}
	// INV: LK-1 — an advisory lock excludes only inside its own database, and
	// the steps run on the pool, so the lock must be held in the pool's
	// database before any step runs.
	if err := confirmLockInPoolDatabase(ctx, pool, lock, req); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		if releaseErr := lock.Release(releaseCtx); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release the table lock for %s: %w", qualifiedName(req.schema, req.table), releaseErr))
		}
		return rep, err
	}
	// Everything the lock protects runs under a context that ends when the
	// lock is lost, so no statement outlives the exclusion it relied on.
	lockedCtx, stop := lock.Bind(ctx)
	rep, err = runPartitionedIndex(lockedCtx, pool, req, sql, b, tracker)
	stop()
	// INV: LK-1 — a lock lost while the build ran voids what it did under the
	// exclusion, whatever else it reported.
	if lost := lock.Err(); lost != nil {
		err = errors.Join(fmt.Errorf("%w (LK-1): %w: %w", ErrInvariantViolation, dbconn.ErrTableLockLost, lost), err)
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
	defer cancel()
	if releaseErr := lock.Release(releaseCtx); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("release the table lock for %s: %w", qualifiedName(req.schema, req.table), releaseErr))
	}
	rep.Duration = elapsedSince(tracker, start)
	return rep, err
}

// confirmLockInPoolDatabase proves, from a pool session, that the backend
// holding the build's table lock holds it in the database the pool is
// connected to. The lock is taken on a session dialed from cfg and every step
// runs on the pool, so a cfg and a pool that reach different databases would
// give the build no exclusion while both looked healthy.
func confirmLockInPoolDatabase(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, req partitionedIndexRequest) error {
	holder, found, err := dbconn.LookupTableLockHolder(ctx, pool, req.schema, req.table)
	if err != nil {
		return fmt.Errorf("confirm the table lock for %s from the pool: %w", qualifiedName(req.schema, req.table), err)
	}
	// A PID is unique only within one server, so the holder's backend start
	// must match too: the pair is what identifies the lock's own session.
	if !found || holder.PID != lock.BackendPID() || !holder.BackendStart.Equal(lock.BackendStart()) {
		return fmt.Errorf("%w: LK-1: the table lock for %s is not held in the database the pool is connected to; the connection target and the pool must reach the same database",
			ErrInvariantViolation, qualifiedName(req.schema, req.table))
	}
	return nil
}

// partitionedIndexRequest is the admitted statement's identity facts.
type partitionedIndexRequest struct {
	schema string
	table  string
	index  string
	// unique and keyColumns are what the partition key check needs: a
	// unique index must contain every partitioned table's partition key
	// among its plain key columns.
	unique     bool
	keyColumns []string
}

// admitPartitionedIndex is the executor's own admission check: exactly one
// named CREATE INDEX, without IF NOT EXISTS, on the schema-qualified table the
// preflight verified, which must be partitioned. Each refusal is typed and
// nothing is executed on one.
func admitPartitionedIndex(sql string, pt preflight.PreflightedTable) (partitionedIndexRequest, error) {
	var req partitionedIndexRequest
	st, err := statement.ParseOne(sql)
	if err != nil {
		return req, err
	}
	if st.Kind() != statement.KindCreateIndex {
		return req, fmt.Errorf("%w: got %s", ErrNotCreateIndex, st.Kind())
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return req, err
	}
	// A CREATE INDEX yields exactly one operation; anything else is
	// parse-boundary version skew and is refused rather than guessed.
	if len(ops) != 1 || ops[0].Kind != statement.OpCreateIndex {
		return req, ErrNotCreateIndex
	}
	switch {
	case ops[0].Name == "":
		return req, ErrUnnamedIndex
	case ops[0].IfNotExists:
		return req, ErrIfNotExistsUnsupported
	case st.Schema() == "":
		return req, ErrUnqualifiedTable
	}
	// INV: ST-7 — the executor runs exactly the statement that was gated, and
	// only against the table the preflight proof verified.
	if st.Table() == "" || st.Schema() != pt.Schema() || st.Table() != pt.Table() {
		return req, fmt.Errorf("%w: ST-7: statement targets %q but preflight verified %q",
			ErrInvariantViolation, qualifiedName(st.Schema(), st.Table()), qualifiedName(pt.Schema(), pt.Table()))
	}
	if !pt.Partitioned() {
		return req, fmt.Errorf("%w: %s", ErrNotPartitionedTable, qualifiedName(pt.Schema(), pt.Table()))
	}
	req = partitionedIndexRequest{schema: st.Schema(), table: st.Table(), index: ops[0].Name, unique: ops[0].Unique}
	if req.unique {
		if req.keyColumns, err = statement.IndexKeyColumns(sql); err != nil {
			return req, fmt.Errorf("read the key columns of index %s: %w", qualifiedName(req.schema, req.index), err)
		}
	}
	return req, nil
}
