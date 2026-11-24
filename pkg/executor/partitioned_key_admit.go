package executor

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdmitPartitionedKey is the catalog half of the partitioned key build's
// admission, for a statement AdmitPartitionedKeyShape admitted: a key that
// does not include every partition key column of every partitioned table
// under the target, or a target partitioned by an expression, is refused with
// a *UniqueIndexPartitionKeyError, the build's own refusal, which the build
// returns under its table lock before it creates anything. A dry run calls it
// to learn that refusal without running the build. It only reads the catalog,
// in one bounded read-only transaction on the pool, which must come from
// pkg/dbconn. The build's other checks under the lock (the parent's
// constraints and columns, the shape of the tree) are not part of it.
func AdmitPartitionedKey(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	st, spec, err := admitPartitionedKeyShape(sql)
	if err != nil {
		return err
	}
	// The read carries the build's own description, which wraps the
	// refusal, so the dry run reports the build's words.
	return briefTx{pool: pool}.read(ctx, "resolve the partitioned table and its partition keys", func(ctx context.Context, q querier) error {
		target, err := resolveTarget(ctx, q, concurrentIndexBuild{tableSchema: st.Schema(), table: st.Table()})
		if err != nil {
			return err
		}
		keys, err := listPartitionKeys(ctx, q, target.tableOID)
		if err != nil {
			return err
		}
		return checkUniquePartitionKeys(qualifiedName(target.schema, spec.name), spec.columns, keys)
	})
}
