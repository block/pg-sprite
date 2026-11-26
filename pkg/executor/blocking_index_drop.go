package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// BlockingIndexDrop records a committed blocking drop of a partitioned
// table's index: the index, the attached partition indexes PostgreSQL
// dropped with it, and how long the transaction ran.
type BlockingIndexDrop struct {
	Schema, Table, Index string
	// IndexOID is the OID of the partitioned index that was dropped.
	IndexOID uint32
	// PartitionIndexes are the attached partition indexes the drop removed
	// with the parent's, schema-qualified, in catalog order.
	PartitionIndexes []string
	Duration         time.Duration
}

// ExecuteBlockingPartitionedIndexDrop drops one reviewed index of the
// partitioned table schema.table with the plain DROP INDEX, the only drop
// PostgreSQL has for a partitioned index. The caller has admitted the drop
// as destructive and accepted its blocking lock; definition is the one the
// plan carried (schemadiff.DroppedObject.Definition).
//
// It runs in one engine-owned transaction under b's lock_timeout and
// statement_timeout and the search_path the plan's definition was read
// under, the schema then public. The table's ACCESS EXCLUSIVE lock is requested first,
// as a statement of its own, which reaches every partition: an exhausted
// lock budget is a *BudgetError with CauseLock and nothing was submitted.
// Under that lock the index is read again and the drop refused, dropping
// nothing, with ErrDroppedIndexDrift unless schema.index is still the
// partitioned index (relkind 'I') of schema.table, not attached under
// another index, with the planned definition; a drop that would remove a
// replica identity index is refused with ErrUnsupportedAcceptedBlocking.
// The DROP INDEX then removes the parent's index and every attached
// partition index, and before the commit the transaction reads back that
// none of them is left; a name another session moved in between rolls the
// drop back as ErrDroppedIndexDrift. A statement-budget cancellation is a
// *BudgetError with CauseStatement, any other server error a
// *BlockingExecutionError, both rolled back, and a commit the client cannot
// confirm a *BlockingOutcomeUnknownError.
func ExecuteBlockingPartitionedIndexDrop(ctx context.Context, pool *pgxpool.Pool, schema, table, index, definition string, b BlockingBudget) (BlockingIndexDrop, error) {
	return executeBlockingPartitionedIndexDrop(ctx, pool, schema, table, index, definition, b, progress.WallClock{})
}

func executeBlockingPartitionedIndexDrop(ctx context.Context, pool *pgxpool.Pool, schema, table, index, definition string,
	b BlockingBudget, clock progress.Clock) (BlockingIndexDrop, error) {
	if err := b.Validate(); err != nil {
		return BlockingIndexDrop{}, err
	}
	if schema == "" || table == "" || index == "" {
		return BlockingIndexDrop{}, fmt.Errorf("%w: a drop needs the schema, table, and index names, got %q, %q, %q",
			ErrUnqualifiedTable, schema, table, index)
	}
	if definition == "" {
		return BlockingIndexDrop{}, fmt.Errorf("%w: dropping index %s.%s needs the definition the plan carried for it",
			ErrInvariantViolation, schema, index)
	}
	ref := pgx.Identifier{schema, index}
	tableRef := pgx.Identifier{schema, table}
	rep := BlockingIndexDrop{Schema: schema, Table: table, Index: index}

	start := clock.Now()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return BlockingIndexDrop{}, fmt.Errorf("begin the blocking drop of %s: %w", ref.Sanitize(), err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// INV: AB-1 — the transaction-local budgets override every session
	// default on the session that drops the index.
	settings := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, settings); err != nil {
		return BlockingIndexDrop{}, fmt.Errorf("set the blocking drop budgets: %w", err)
	}
	// The index is read under the search_path the plan's definition was read
	// under, the schema then public, so an expression's function or an
	// operator class in the schema prints alike in both reads.
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(schema, "public")); err != nil {
		return BlockingIndexDrop{}, fmt.Errorf("set the blocking drop search_path: %w", err)
	}
	// INV: AB-2 — the parent's lock, which recurses to every partition, is
	// requested before anything is submitted, so an exhausted lock budget
	// drops nothing.
	if _, err := tx.Exec(ctx, "LOCK TABLE "+tableRef.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return BlockingIndexDrop{}, acceptedBlockingLockError(ctx, err, b)
	}
	oid, err := readPartitionedIndexToDrop(ctx, tx, schema, table, index, definition)
	if err != nil {
		return BlockingIndexDrop{}, err
	}
	if err := refuseReplicaIdentityDrop(ctx, tx, statement.IndexRelation{Schema: schema, Name: index, Kind: statement.IndexRelationIndex}); err != nil {
		return BlockingIndexDrop{}, err
	}
	partitionOIDs, partitionNames, err := attachedPartitionIndexes(ctx, tx, oid)
	if err != nil {
		return BlockingIndexDrop{}, err
	}

	// Never CONCURRENTLY: PostgreSQL rejects it for a partitioned index.
	if _, err := tx.Exec(ctx, "DROP INDEX "+ref.Sanitize()); err != nil {
		return BlockingIndexDrop{}, acceptedBlockingStatementError(ctx, err, b)
	}
	if err := confirmIndexesDropped(ctx, tx, ref, oid, partitionOIDs); err != nil {
		return BlockingIndexDrop{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BlockingIndexDrop{}, &BlockingOutcomeUnknownError{Err: err}
	}
	rep.IndexOID = oid
	rep.PartitionIndexes = partitionNames
	rep.Duration = clock.Now().Sub(start)
	return rep, nil
}

// readPartitionedIndexToDrop reads, under the table's lock, the index a
// reviewed plan drops and returns its OID, or ErrDroppedIndexDrift when it
// is no longer the partitioned index of schema.table with the planned
// definition. An index attached under another index is refused the same
// way: PostgreSQL drops it only with its parent, and a desired plan never
// names one.
func readPartitionedIndexToDrop(ctx context.Context, tx pgx.Tx, schema, table, index, definition string) (uint32, error) {
	ref := pgx.Identifier{schema, index}.Sanitize()
	tableRef := pgx.Identifier{schema, table}.Sanitize()
	var (
		oid                   uint32
		relkind, tableRelkind string
		attached              bool
	)
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	err := tx.QueryRow(ctx, `
		SELECT c.oid, c.relkind::pg_catalog.text, c.relispartition, t.relkind::pg_catalog.text
		  FROM pg_catalog.pg_index i
		  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		  JOIN pg_catalog.pg_namespace tn ON tn.oid OPERATOR(pg_catalog.=) t.relnamespace
		 WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
		   AND tn.nspname OPERATOR(pg_catalog.=) $1 AND t.relname OPERATOR(pg_catalog.=) $3`,
		schema, index, table).Scan(&oid, &relkind, &attached, &tableRelkind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("%w: %s has no index %s on %s any more; re-plan", ErrDroppedIndexDrift, schema, ref, tableRef)
	case err != nil:
		return 0, fmt.Errorf("read index %s before dropping it: %w", ref, err)
	}
	switch {
	case tableRelkind != "p" || relkind != "I":
		return 0, fmt.Errorf("%w: %s is no longer an index of the partitioned table %s, which the plan reviewed; re-plan",
			ErrDroppedIndexDrift, ref, tableRef)
	case attached:
		return 0, fmt.Errorf("%w: %s is attached under another partitioned index, and PostgreSQL drops it only with that index; re-plan",
			ErrDroppedIndexDrift, ref)
	}
	live, found, err := schemadiff.ReadDroppedObject(ctx, tx, schema, table, schemadiff.DroppedIndex, index)
	switch {
	case err != nil:
		return 0, fmt.Errorf("read the definition of index %s before dropping it: %w", ref, err)
	case !found:
		return 0, fmt.Errorf("%w: %s has no index %s on %s any more; re-plan", ErrDroppedIndexDrift, schema, ref, tableRef)
	case live.Definition != definition:
		return 0, fmt.Errorf("%w: index %s is now %q, not the %q the plan reviewed; re-plan",
			ErrDroppedIndexDrift, ref, live.Definition, definition)
	}
	return oid, nil
}

// attachedPartitionIndexes lists the indexes attached, at any depth, under
// the partitioned index oid: the ones its DROP INDEX removes with it.
func attachedPartitionIndexes(ctx context.Context, tx pgx.Tx, oid uint32) ([]uint32, []string, error) {
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	rows, err := tx.Query(ctx, `
		SELECT c.oid, pg_catalog.quote_ident(n.nspname) OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident(c.relname)
		  FROM pg_catalog.pg_partition_tree($1::pg_catalog.oid::pg_catalog.regclass) tree
		  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) tree.relid
		  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		 WHERE c.oid OPERATOR(pg_catalog.<>) $1::pg_catalog.oid
		 ORDER BY tree.level, c.oid`, oid)
	if err != nil {
		return nil, nil, fmt.Errorf("list the partition indexes of index oid %d: %w", oid, err)
	}
	var (
		oids  []uint32
		names []string
	)
	for rows.Next() {
		var (
			partOID uint32
			name    string
		)
		if err := rows.Scan(&partOID, &name); err != nil {
			return nil, nil, fmt.Errorf("read a partition index of index oid %d: %w", oid, err)
		}
		oids = append(oids, partOID)
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list the partition indexes of index oid %d: %w", oid, err)
	}
	return oids, names, nil
}

// confirmIndexesDropped reads back, inside the drop's transaction, that the
// index that was checked and every partition index attached under it are
// gone. PostgreSQL drops an index only by name, so a checked index still in
// the catalog means another session gave the name to a different index in
// between; the drop is rolled back and nothing was dropped.
func confirmIndexesDropped(ctx context.Context, tx pgx.Tx, ref pgx.Identifier, oid uint32, partitionOIDs []uint32) error {
	var parentLeft bool
	var partitionsLeft int
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE oid OPERATOR(pg_catalog.=) $1),
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_class WHERE oid OPERATOR(pg_catalog.=) ANY ($2::pg_catalog.oid[]))`,
		oid, partitionOIDs).Scan(&parentLeft, &partitionsLeft)
	if err != nil {
		return fmt.Errorf("confirm the drop of %s removed it and its partition indexes: %w", ref.Sanitize(), err)
	}
	if parentLeft {
		return fmt.Errorf("%w: the index the plan checked (oid %d) is still in the catalog after DROP INDEX %s, so another session "+
			"gave the name to a different index; the drop was rolled back; re-plan", ErrDroppedIndexDrift, oid, ref.Sanitize())
	}
	if partitionsLeft != 0 {
		return fmt.Errorf("%w: DROP INDEX %s left %d of its %d partition indexes in the catalog; the drop was rolled back",
			ErrInvariantViolation, ref.Sanitize(), partitionsLeft, len(partitionOIDs))
	}
	return nil
}
