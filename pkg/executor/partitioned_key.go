// This file is the partitioned key build: the online way to add a PRIMARY KEY
// or UNIQUE constraint to a partitioned table. A plain ALTER TABLE ... ADD
// CONSTRAINT on a partitioned table builds every partition's index while it
// holds ACCESS EXCLUSIVE on the tree, and PostgreSQL refuses ADD CONSTRAINT
// ... USING INDEX on one. The same plain ADD CONSTRAINT adopts a partition's
// constraint of the requested definition instead of building one, so the
// build first gives every leaf that constraint online (ensureLeafKey), then
// runs the parent's ADD CONSTRAINT, which only changes the catalog
// (docs/partitioned-index-build-design.md).

package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// PartitionKeyAction says what the build did to give one partition the key
// constraint the parent's constraint adopts.
type PartitionKeyAction string

const (
	// PartitionKeyFound means the partition already had the requested
	// constraint, attached to no parent's constraint: an earlier run's, or
	// one an operator made.
	PartitionKeyFound PartitionKeyAction = "found"
	// PartitionKeyConstrained means the partition already had a valid unique
	// index of the key's definition, and the build gave it the constraint.
	PartitionKeyConstrained PartitionKeyAction = "constrained"
	// PartitionKeyBuilt means the build created the partition's index
	// concurrently, then gave it the constraint.
	PartitionKeyBuilt PartitionKeyAction = "built"
)

// PartitionKeyResult is one leaf partition's key constraint as the build left
// it for the parent's constraint to adopt.
type PartitionKeyResult struct {
	// Schema and Table identify the partition.
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// Constraint and ConstraintOID identify the partition's constraint.
	Constraint    string `json:"constraint"`
	ConstraintOID uint32 `json:"constraint_oid"`
	// Index and IndexOID identify the index behind it. The parent's
	// constraint adopts the index as it is, so its OID does not change.
	Index    string `json:"index"`
	IndexOID uint32 `json:"index_oid"`
	// Action says how the partition got its constraint.
	Action PartitionKeyAction `json:"action"`
	// RecoveredDebris is true when the build first removed the invalid
	// leftover of an earlier failed build of the partition's index.
	RecoveredDebris bool `json:"recovered_debris,omitempty"`
}

// PartitionedKeyReport says what a partitioned key build did. It is returned
// only after the executor read back, in the transaction that added it, that
// the parent's constraint adopted every partition's constraint and index
// without building any.
type PartitionedKeyReport struct {
	// Schema and Table identify the partitioned table.
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// Constraint and ConstraintOID identify the parent's constraint.
	Constraint    string `json:"constraint"`
	ConstraintOID uint32 `json:"constraint_oid"`
	// ParentAdded is true when this run added the parent's constraint, and
	// false when an earlier run had already added it, which this run found
	// and reported.
	ParentAdded bool `json:"parent_added"`
	// Partitions is the number of partitions in the tree.
	Partitions int `json:"partitions"`
	// Leaves lists every partition the run gave the key, in the order it
	// first reached them. It is empty when the parent's constraint already
	// existed.
	Leaves []PartitionKeyResult `json:"leaves,omitempty"`
	// Passes is how many times the build walked the tree.
	Passes int `json:"passes"`
	// Duration is the wall-clock time of the whole build. It encodes as
	// integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
	// ServerVersion is the server_version of the server that ran the build.
	ServerVersion string `json:"server_version"`
}

// maxPartitionedKeyPasses bounds how many times the build walks the partition
// tree. A pass gives every listed leaf its key and then adds the parent's
// constraint; it defers the parent to another pass only when a partition
// appeared that it did not cover, and the bound is the room other sessions'
// partition changes get before the build refuses instead of chasing them.
const maxPartitionedKeyPasses = 5

// AddPartitionedKey adds the PRIMARY KEY or UNIQUE constraint an ALTER TABLE
// statement describes to a partitioned table, online, and returns once the
// parent's constraint exists and has adopted a constraint on every partition.
//
// The statement is one schema-qualified ALTER TABLE parent ADD CONSTRAINT
// name {PRIMARY KEY | UNIQUE} (columns), on the preflighted partitioned table,
// with no other clause. The build holds the parent's advisory table lock
// (LK-1) for its whole run, so a second run on the same parent is refused
// with a *dbconn.TableLockHeldError; cfg is the connection target that lock's
// dedicated session dials, the same Config the pool was made from. Under the
// lock it:
//
//  1. refuses a key that lacks a partition key column
//     (*UniqueIndexPartitionKeyError), a parent that already has a different
//     constraint under the name, an index under it, or, for a primary key,
//     another primary key (ErrPartitionedKeyParentConflict), and a primary
//     key over a column the parent allows to be NULL
//     (ErrPartitionedKeyNullableColumn); a parent that already has exactly
//     the requested constraint is reported done;
//  2. refuses a tree that holds a foreign table (ErrForeignTablePartition), a
//     sub-partitioned partition (ErrPartitionedKeySubPartitioned), or a
//     partition whose DETACH PARTITION CONCURRENTLY has not finished
//     (ErrPartitionDetachPending), before any partition is touched;
//  3. gives each leaf the key constraint: a concurrent build of its unique
//     index, then ADD CONSTRAINT ... USING INDEX, resumed from whatever an
//     earlier run left;
//  4. in one brief transaction, locks the tree, proves every partition has
//     exactly the constraint and index step 3 recorded, runs the parent's
//     ADD CONSTRAINT, and reads back that it adopted each of them without
//     building an index; a partition that appeared in the meantime sends the
//     build round again, up to a fixed number of passes
//     (ErrPartitionedKeyUnsettled when they keep appearing).
//
// A failure at any point leaves every finished partition's constraint, and
// rerunning the same statement resumes from them. Every partition's build is
// bounded by b, one budget per partition; every other statement runs under
// the pool's own bounded lock and statement timeouts. The pool must come from
// pkg/dbconn and hold at least two connections, the concurrent build's own
// minimum.
func AddPartitionedKey(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ConcurrentBudget) (PartitionedKeyReport, error) {
	return addPartitionedKeyAt(ctx, cfg, pool, pt, sql, b, nil, 0)
}

// AddPartitionedKeyAsStep is AddPartitionedKey reported to the run step at:
// the whole build is step at.Step of a run that owns the tracker, so each
// partition's build and the parent's constraint are each a statement of that
// one step, and the run's step total is untouched. The call never calls Start
// or Finish.
func AddPartitionedKeyAsStep(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ConcurrentBudget, at RunStep) (PartitionedKeyReport, error) {
	if err := at.validate(); err != nil {
		return PartitionedKeyReport{}, err
	}
	return addPartitionedKeyAt(ctx, cfg, pool, pt, sql, b, at.Tracker, at.Step)
}

func addPartitionedKeyAt(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ConcurrentBudget, tracker *progress.Tracker, runStep int) (PartitionedKeyReport, error) {
	var rep PartitionedKeyReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	// INV: LK-2 — caller-owned mode is bounded by the caller's cancellation
	// signal, so a context without one is refused before any session use.
	if b.CallerOwned && ctx.Done() == nil {
		return rep, ErrCallerOwnedNeedsCancellableContext
	}
	req, err := admitPartitionedKey(sql, pt)
	if err != nil {
		return rep, err
	}
	// INV: LK-2 — a partition's build needs its session and the reserved
	// verdict session at once; a smaller pool would make every failed build
	// indeterminate or wait on itself.
	if pool.Config().MaxConns < buildMinConns {
		return rep, fmt.Errorf("partitioned key build needs %d connections (build session and reserved verdict session), pool holds %d: %w",
			buildMinConns, pool.Config().MaxConns, ErrPoolTooSmall)
	}

	start := time.Now()
	if tracker != nil {
		start = tracker.Now()
	}
	ran := false
	err = runUnderParentLock(ctx, cfg, pool, req.schema, req.table, "the partitioned key build", func(lockedCtx context.Context) error {
		ran = true
		var runErr error
		rep, runErr = runPartitionedKey(lockedCtx, pool, req, b, tracker, runStep)
		return runErr
	})
	// A build refused before it held the lock ran nothing and reports no
	// duration.
	if !ran {
		return rep, err
	}
	rep.Duration = elapsedSince(tracker, start)
	return rep, err
}

// partitionedKeyRequest is the admitted statement: the parent it targets and
// the key it asks for.
type partitionedKeyRequest struct {
	schema string
	table  string
	spec   partitionedKeySpec
}

// admitPartitionedKey is the executor's own admission check: the statement's
// shape, on the schema-qualified table the preflight verified, which must be
// partitioned. Each refusal is typed and nothing is executed on one.
func admitPartitionedKey(sql string, pt preflight.PreflightedTable) (partitionedKeyRequest, error) {
	st, spec, err := admitPartitionedKeyShape(sql)
	if err != nil {
		return partitionedKeyRequest{}, err
	}
	// INV: ST-7 — the executor runs exactly the statement that was gated, and
	// only against the table the preflight proof verified.
	if st.Schema() != pt.Schema() || st.Table() != pt.Table() {
		return partitionedKeyRequest{}, fmt.Errorf("%w: ST-7: statement targets %q but preflight verified %q",
			ErrInvariantViolation, qualifiedName(st.Schema(), st.Table()), qualifiedName(pt.Schema(), pt.Table()))
	}
	if !pt.Partitioned() {
		return partitionedKeyRequest{}, fmt.Errorf("%w: %s", ErrNotPartitionedTable, qualifiedName(pt.Schema(), pt.Table()))
	}
	return partitionedKeyRequest{schema: st.Schema(), table: st.Table(), spec: spec}, nil
}

// AdmitPartitionedKeyShape is the statement-shape half of the partitioned key
// build's admission: exactly one schema-qualified ALTER TABLE with one ADD
// CONSTRAINT name {PRIMARY KEY | UNIQUE} over a plain list of distinct
// columns, and no other clause. It needs no connection, so a dry run learns
// the refusals the build would return before it runs anything. The refusals
// are the build's own, typed.
func AdmitPartitionedKeyShape(sql string) error {
	_, _, err := admitPartitionedKeyShape(sql)
	return err
}

// admitPartitionedKeyShape returns the parsed statement and the key it asks
// for. The build never runs the statement's text: the parent's ADD CONSTRAINT
// is rendered from the spec, which can hold nothing but the kind, the name,
// and the columns, so every clause the spec cannot hold is refused here.
func admitPartitionedKeyShape(sql string) (statement.Statement, partitionedKeySpec, error) {
	st, err := statement.ParseOne(sql)
	if err != nil {
		return statement.Statement{}, partitionedKeySpec{}, err
	}
	if st.Kind() != statement.KindAlterTable {
		return statement.Statement{}, partitionedKeySpec{}, fmt.Errorf("%w: got %s", ErrNotAddKeyConstraint, st.Kind())
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return statement.Statement{}, partitionedKeySpec{}, err
	}
	if len(ops) != 1 {
		return statement.Statement{}, partitionedKeySpec{}, fmt.Errorf("%w: the statement has %d subcommands", ErrNotAddKeyConstraint, len(ops))
	}
	op := ops[0]
	kind, ok := keyKindOf(op)
	if !ok {
		return statement.Statement{}, partitionedKeySpec{}, ErrNotAddKeyConstraint
	}
	if st.Schema() == "" {
		return statement.Statement{}, partitionedKeySpec{}, ErrUnqualifiedTable
	}
	if op.Name == "" {
		return statement.Statement{}, partitionedKeySpec{}, fmt.Errorf("%w: the constraint has no name; name it, because each partition's index and constraint take names derived from it and a rerun finds them by those names",
			ErrPartitionedKeyUnsupportedShape)
	}
	if clauses := unsupportedKeyClauses(op); len(clauses) > 0 {
		return statement.Statement{}, partitionedKeySpec{}, fmt.Errorf("%w: constraint %s has %s",
			ErrPartitionedKeyUnsupportedShape, op.Name, strings.Join(clauses, ", "))
	}
	return st, partitionedKeySpec{kind: kind, name: op.Name, columns: op.Columns}, nil
}

// keyKindOf returns the key kind an ADD CONSTRAINT adds, and false for any
// other operation.
func keyKindOf(op statement.Op) (partitionedKeyKind, bool) {
	if op.Kind != statement.OpAddConstraint {
		return 0, false
	}
	switch op.Constraint {
	case statement.ConstraintPrimaryKey:
		return partitionedPrimaryKey, true
	case statement.ConstraintUnique:
		return partitionedUniqueKey, true
	default:
		return 0, false
	}
}

// unsupportedKeyClauses names every clause of an ADD PRIMARY KEY or ADD
// UNIQUE that the build cannot carry to the partitions' constraints.
func unsupportedKeyClauses(op statement.Op) []string {
	var clauses []string
	if op.Only {
		clauses = append(clauses, "ONLY, which would add the parent's constraint without the partitions'")
	}
	if op.UsingIndex {
		clauses = append(clauses, "USING INDEX, which PostgreSQL refuses on a partitioned table")
	}
	if len(op.IncludeColumns) > 0 {
		clauses = append(clauses, "INCLUDE")
	}
	if op.NullsNotDistinct {
		clauses = append(clauses, "NULLS NOT DISTINCT")
	}
	if op.Deferrable {
		clauses = append(clauses, "DEFERRABLE")
	}
	if op.InitiallyDeferred {
		clauses = append(clauses, "INITIALLY DEFERRED")
	}
	if len(op.IndexStorageParams) > 0 || op.IndexOptionsUnrenderable {
		clauses = append(clauses, "WITH (...) storage parameters")
	}
	if op.IndexTablespace != "" {
		clauses = append(clauses, "USING INDEX TABLESPACE "+op.IndexTablespace)
	}
	if op.NotValid {
		clauses = append(clauses, "NOT VALID")
	}
	if len(op.Columns) == 0 && !op.UsingIndex {
		clauses = append(clauses, "no plain column list")
	}
	seen := make(map[string]struct{}, len(op.Columns))
	for _, c := range op.Columns {
		if _, dup := seen[c]; dup {
			clauses = append(clauses, fmt.Sprintf("column %q named twice", c))
		}
		seen[c] = struct{}{}
	}
	return clauses
}
