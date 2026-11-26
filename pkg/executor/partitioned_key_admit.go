package executor

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdmitPartitionedKey is the catalog half of the partitioned key build's
// admission, for a statement AdmitPartitionedKeyShape admitted: a key that
// does not include every partition key column of every partitioned table
// under the target, or a target partitioned by an expression, is refused with
// a *UniqueIndexPartitionKeyError, and a tree with a sub-partitioned
// partition is refused with ErrPartitionedKeySubPartitioned. These are the
// build's own refusals, which the build returns under its table lock before
// it creates anything. A dry run calls it to learn them without running the
// build. It only reads the catalog, in one bounded read-only transaction on
// the pool, which must come from pkg/dbconn. The build's checks of the
// parent's constraints and columns, and its refusal of a foreign table or a
// partition whose detach has not finished, are not part of it, so a dry run
// does not claim them; the build reads them under its lock.
func AdmitPartitionedKey(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	st, spec, err := admitPartitionedKeyShape(sql)
	if err != nil {
		return err
	}
	// The read carries the build's own description, which wraps the
	// refusal, so the dry run reports the build's words.
	var (
		target indexTarget
		tree   []partitionNode
	)
	err = briefTx{pool: pool}.read(ctx, "resolve the partitioned table and its partition keys", func(ctx context.Context, q querier) error {
		var err error
		if target, err = resolveTarget(ctx, q, concurrentIndexBuild{tableSchema: st.Schema(), table: st.Table()}); err != nil {
			return err
		}
		if tree, err = listPartitionTree(ctx, q, target.tableOID); err != nil {
			return err
		}
		return readUncoveredPartitionKeys(ctx, q, target.schema, spec.name, spec.columns, keyedTables(target, tree))
	})
	if err != nil {
		return err
	}
	// The build refuses the tree outside any read, as here, so the dry run
	// reports its words for this refusal too.
	return refuseSubPartitioned(target, tree)
}
