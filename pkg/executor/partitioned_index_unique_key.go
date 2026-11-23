package executor

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// A unique index on a partitioned table can only enforce uniqueness inside
// each partition, so PostgreSQL requires its key to include every column of
// the table's partition key, and refuses a partition key that is an
// expression outright. It checks this on every partitioned table the index is
// created on: the parent when its ON ONLY index is created, and each
// sub-partitioned partition when the build gets to it. The build checks the
// whole tree before it creates the parent's index, so a key the server would
// reject part-way down the tree is refused with nothing created.

// UniqueIndexPartitionKeyError reports a unique index whose key does not
// include every partition key column of every partitioned table in the tree.
// It unwraps to ErrUniqueIndexMissingPartitionKey.
type UniqueIndexPartitionKeyError struct {
	// Index is the requested index, schema-qualified.
	Index string
	// Missing lists, parents before their partitions, each partitioned table
	// whose partition key the index does not cover.
	Missing []MissingPartitionKey
}

// MissingPartitionKey is one partitioned table whose partition key a unique
// index does not cover.
type MissingPartitionKey struct {
	// Table is the partitioned table, schema-qualified.
	Table string
	// Columns are its partition key columns the index's key lacks, in key
	// order.
	Columns []string
	// Expression is true when its partition key includes an expression,
	// which no unique index can cover.
	Expression bool
}

// Error implements the error interface.
func (e *UniqueIndexPartitionKeyError) Error() string {
	parts := make([]string, 0, len(e.Missing))
	for _, m := range e.Missing {
		if len(m.Columns) > 0 {
			parts = append(parts, fmt.Sprintf("it lacks partition key column(s) %s of %s", strings.Join(m.Columns, ", "), m.Table))
		}
		if m.Expression {
			parts = append(parts, fmt.Sprintf("%s is partitioned by an expression, which no unique index can include", m.Table))
		}
	}
	return fmt.Sprintf("%v: unique index %s cannot be created: %s", ErrUniqueIndexMissingPartitionKey, e.Index, strings.Join(parts, "; "))
}

// Unwrap returns ErrUniqueIndexMissingPartitionKey.
func (e *UniqueIndexPartitionKeyError) Unwrap() error { return ErrUniqueIndexMissingPartitionKey }

// partitionKey is one partitioned table in a tree and its partition key as
// the catalog records it.
type partitionKey struct {
	table string
	// columns are the key's plain columns, in key order.
	columns []string
	// expression is true when the key includes an expression.
	expression bool
}

// listPartitionKeys returns the partition key of the partitioned table with
// the given OID and of every partitioned table under it, parents before their
// partitions.
func listPartitionKeys(ctx context.Context, q querier, tableOID uint32) ([]partitionKey, error) {
	rows, err := q.Query(ctx, listPartitionKeysSQL, tableOID)
	if err != nil {
		return nil, fmt.Errorf("list partition keys under table %d: %w", tableOID, err)
	}
	defer rows.Close()
	var keys []partitionKey
	for rows.Next() {
		var (
			k            partitionKey
			schema, name string
		)
		if err := rows.Scan(&schema, &name, &k.columns, &k.expression); err != nil {
			return nil, fmt.Errorf("scan partition keys under table %d: %w", tableOID, err)
		}
		k.table = qualifiedName(schema, name)
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list partition keys under table %d: %w", tableOID, err)
	}
	return keys, nil
}

// listPartitionKeysSQL lists the partition key of the table $1 and of each
// partitioned table under it. pg_partitioned_table.partattrs holds a column
// number for each key position and 0 for an expression, whose text is in
// partexprs; only the columns are named here.
const listPartitionKeysSQL = `WITH RECURSIVE tree AS (
		        SELECT $1::pg_catalog.oid AS oid, 0 AS depth
		        UNION ALL
		        SELECT h.inhrelid, tree.depth OPERATOR(pg_catalog.+) 1
		          FROM pg_catalog.pg_inherits h
		          JOIN tree ON h.inhparent OPERATOR(pg_catalog.=) tree.oid)
		 SELECT n.nspname, c.relname,
		        ARRAY(
		            SELECT a.attname::pg_catalog.text
		              FROM pg_catalog.unnest(pt.partattrs::pg_catalog.int2[]) WITH ORDINALITY AS k(attnum, ord)
		              JOIN pg_catalog.pg_attribute a
		                ON a.attrelid OPERATOR(pg_catalog.=) pt.partrelid
		               AND a.attnum OPERATOR(pg_catalog.=) k.attnum
		             ORDER BY k.ord),
		        pt.partexprs IS NOT NULL
		   FROM tree
		   JOIN pg_catalog.pg_partitioned_table pt ON pt.partrelid OPERATOR(pg_catalog.=) tree.oid
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) tree.oid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  ORDER BY tree.depth, tree.oid`

// uniqueIndexKey returns whether sql, an admitted CREATE INDEX, is unique and,
// when it is, the columns its key names.
func uniqueIndexKey(sql string) (bool, []string, error) {
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return false, nil, err
	}
	if len(ops) != 1 || ops[0].Kind != statement.OpCreateIndex {
		return false, nil, ErrNotCreateIndex
	}
	if !ops[0].Unique {
		return false, nil, nil
	}
	columns, err := statement.IndexKeyColumns(sql)
	if err != nil {
		return false, nil, err
	}
	return true, columns, nil
}

// checkUniquePartitionKeys refuses a unique index whose key columns do not
// include every partition key column of every table in keys, matching a
// partition key column to an index key column by name, as the partitions of a
// tree name their columns alike. The server also requires the two to share a
// collation and an equality operator; it checks that itself, and the parent's
// ON ONLY step is where it would refuse.
func checkUniquePartitionKeys(index string, indexColumns []string, keys []partitionKey) error {
	var missing []MissingPartitionKey
	for _, k := range keys {
		var lacks []string
		for _, column := range k.columns {
			if !slices.Contains(indexColumns, column) {
				lacks = append(lacks, column)
			}
		}
		if len(lacks) == 0 && !k.expression {
			continue
		}
		missing = append(missing, MissingPartitionKey{Table: k.table, Columns: lacks, Expression: k.expression})
	}
	if len(missing) == 0 {
		return nil
	}
	return &UniqueIndexPartitionKeyError{Index: index, Missing: missing}
}

// AdmitPartitionedUniqueIndex is the catalog half of the partitioned index
// build's admission, for a statement AdmitPartitionedIndexShape admitted: a
// unique index whose key does not include every partition key column of every
// partitioned table under the target is refused with a
// *UniqueIndexPartitionKeyError, the build's own refusal, which the build
// returns under its table lock before it creates anything. A dry run calls it
// to learn that refusal without running the build. It only reads the catalog,
// in one bounded read-only transaction on the pool, which must come from
// pkg/dbconn. A non-unique index is admitted without a read.
func AdmitPartitionedUniqueIndex(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	st, index, err := admitPartitionedIndexShape(sql)
	if err != nil {
		return err
	}
	unique, columns, err := uniqueIndexKey(sql)
	if err != nil {
		return err
	}
	if !unique {
		return nil
	}
	var (
		target indexTarget
		keys   []partitionKey
	)
	err = briefTx{pool: pool}.read(ctx, "read the partition keys", func(ctx context.Context, q querier) error {
		var err error
		if target, err = resolveTarget(ctx, q, concurrentIndexBuild{index: index, tableSchema: st.Schema(), table: st.Table()}); err != nil {
			return err
		}
		keys, err = listPartitionKeys(ctx, q, target.tableOID)
		return err
	})
	if err != nil {
		return err
	}
	return checkUniquePartitionKeys(qualifiedName(target.schema, index), columns, keys)
}

// checkUniqueIndex is the build's own check of a unique index against the
// partition keys of the tree, read under the table lock before the parent's
// index is created.
func (f *partitionedIndexFlow) checkUniqueIndex(ctx context.Context) error {
	unique, columns, err := uniqueIndexKey(f.sql)
	if err != nil {
		return err
	}
	if !unique {
		return nil
	}
	var keys []partitionKey
	err = f.tx.read(ctx, "read the partition keys", func(ctx context.Context, q querier) error {
		var err error
		keys, err = listPartitionKeys(ctx, q, f.target.tableOID)
		return err
	})
	if err != nil {
		return err
	}
	// The refusal is returned as it is, unwrapped, so the dry run's
	// AdmitPartitionedUniqueIndex reports the same words.
	return checkUniquePartitionKeys(qualifiedName(f.target.schema, f.req.index), columns, keys)
}
