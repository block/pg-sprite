package executor

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdmitPartitionedForeignKey is the catalog half of the partitioned foreign
// key build's admission, for a statement AdmitPartitionedForeignKeyShape
// admitted: a referenced table that is not a plain table (partitioned,
// foreign, a view, or the target itself) or that is a partition of the
// target is refused with ErrPartitionedForeignKeyReferencedTable, and a tree
// holding a foreign table (ErrForeignTablePartition), a partition whose
// detach has not finished (ErrPartitionDetachPending), or a sub-partitioned
// partition (ErrPartitionedForeignKeySubPartitioned) is refused as well.
// These are the build's own refusals, which the build returns under its
// table lock before it gives any partition the foreign key. A desired-state
// run calls it for the whole plan before any statement runs, and a dry run
// calls it to learn them without running the build. It only reads the
// catalog, in one bounded read-only transaction on the pool, which must come
// from pkg/dbconn, and takes no table lock. The build's checks of the
// server version, the parent's constraints, and the columns' attribute
// numbers on each partition are not part of it, so a caller does not claim
// them; the build reads them under its lock.
func AdmitPartitionedForeignKey(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	st, spec, err := admitPartitionedFKShape(sql)
	if err != nil {
		return err
	}
	// The read carries the build's own description, which wraps the
	// refusal, so the caller reports the build's words.
	var (
		target indexTarget
		ref    fkReference
		tree   []partitionNode
	)
	err = briefTx{pool: pool}.read(ctx, "resolve the partitioned table and the referenced table", func(ctx context.Context, q querier) error {
		var err error
		if target, err = resolveTarget(ctx, q, concurrentIndexBuild{tableSchema: st.Schema(), table: st.Table()}); err != nil {
			return err
		}
		if ref, err = resolveFKReference(ctx, q, spec); err != nil {
			return err
		}
		tree, err = listPartitionTree(ctx, q, target.tableOID)
		return err
	})
	if err != nil {
		return err
	}
	// The build refuses the tree outside any read, as here, so the caller
	// reports its words for these refusals too.
	return refuseFKTree(target, ref, tree)
}
