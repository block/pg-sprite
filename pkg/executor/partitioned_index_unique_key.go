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
// each partition, so PostgreSQL requires its plain key columns to contain the
// partition key of every partitioned table it is created on, and refuses a
// partition key that is an expression outright. It checks one partitioned
// table at a time, as each gets its index, so the build checks the parent and
// every sub-partitioned partition first and refuses with nothing created.
//
// The check matches columns by name, as the partitions of a tree name their
// columns alike. The server also requires each matched column to share the
// partition key's collation and equality operator; on the parent it refuses a
// mismatch at the ON ONLY step, which creates nothing, but on a
// sub-partitioned partition it refuses only at that partition's step, after
// the parent's invalid index exists, and a rerun resumes into that index and
// fails at the same step.

// UniqueIndexPartitionKeyError reports a unique index whose plain key columns
// do not contain the partition key of every partitioned table in the tree. It
// unwraps to ErrUniqueIndexPartitionKey.
type UniqueIndexPartitionKeyError struct {
	// Schema and Index name the requested index.
	Schema string
	Index  string
	// Missing lists, parents before their partitions, each partitioned table
	// whose partition key the index does not contain.
	Missing []MissingPartitionKey
}

// MissingPartitionKey is one partitioned table whose partition key a unique
// index does not contain.
type MissingPartitionKey struct {
	// Schema and Table name the partitioned table.
	Schema string
	Table  string
	// Columns are its partition key columns the index's key lacks, in key
	// order.
	Columns []string
	// Expression is true when its partition key includes an expression,
	// which no unique index can contain.
	Expression bool
}

// Error implements the error interface.
func (e *UniqueIndexPartitionKeyError) Error() string {
	parts := make([]string, 0, len(e.Missing))
	for _, m := range e.Missing {
		table := qualifiedName(m.Schema, m.Table)
		if len(m.Columns) > 0 {
			parts = append(parts, fmt.Sprintf("it lacks partition key column(s) %s of %s", strings.Join(m.Columns, ", "), table))
		}
		if m.Expression {
			parts = append(parts, fmt.Sprintf("%s is partitioned by an expression, which no unique index can contain", table))
		}
	}
	return fmt.Sprintf("%v: unique index %s cannot be created: %s",
		ErrUniqueIndexPartitionKey, qualifiedName(e.Schema, e.Index), strings.Join(parts, "; "))
}

// Unwrap returns ErrUniqueIndexPartitionKey.
func (e *UniqueIndexPartitionKeyError) Unwrap() error { return ErrUniqueIndexPartitionKey }

// keyedTable is a partitioned table of the tree whose partition key a unique
// index must contain.
type keyedTable struct {
	oid    uint32
	schema string
	name   string
}

// keyedTables lists the target and every sub-partitioned partition of tree,
// parents before their partitions, as listPartitionTree orders the tree.
func keyedTables(target indexTarget, tree []partitionNode) []keyedTable {
	tables := []keyedTable{{oid: target.tableOID, schema: target.schema, name: target.table}}
	for _, n := range tree {
		if n.subPartitioned() {
			tables = append(tables, keyedTable{oid: n.oid, schema: n.schema, name: n.name})
		}
	}
	return tables
}

// readUncoveredPartitionKeys reads the partition keys of tables and returns
// the *UniqueIndexPartitionKeyError for the index whose plain key columns are
// keyColumns, or nil when it contains every one.
func readUncoveredPartitionKeys(ctx context.Context, q querier, schema, index string, keyColumns []string, tables []keyedTable) error {
	oids := make([]uint32, 0, len(tables))
	for _, t := range tables {
		oids = append(oids, t.oid)
	}
	keys, err := partitionKeyColumns(ctx, q, oids)
	if err != nil {
		return err
	}
	return checkUniquePartitionKeys(schema, index, keyColumns, tables, keys)
}

// checkUniquePartitionKeys compares the index's plain key columns with each
// table's partition key, keyed by table OID, in which an expression is the
// empty string. Columns match by exact name.
func checkUniquePartitionKeys(schema, index string, keyColumns []string, tables []keyedTable, keys map[uint32][]string) error {
	var missing []MissingPartitionKey
	for _, t := range tables {
		m := MissingPartitionKey{Schema: t.schema, Table: t.name}
		for _, column := range keys[t.oid] {
			switch {
			case column == "":
				m.Expression = true
			case !slices.Contains(keyColumns, column):
				m.Columns = append(m.Columns, column)
			}
		}
		if len(m.Columns) > 0 || m.Expression {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &UniqueIndexPartitionKeyError{Schema: schema, Index: index, Missing: missing}
}

// AdmitPartitionedUniqueIndex is the catalog half of the partitioned index
// build's admission, for a statement AdmitPartitionedIndexShape admitted: a
// unique index whose plain key columns do not contain the partition key of
// the target and of every sub-partitioned partition is refused with the
// build's own *UniqueIndexPartitionKeyError, which the build returns under
// its table lock before it creates anything. A dry run calls it to learn that
// refusal without running the build. It only reads the catalog, in bounded
// transactions on the pool that always roll back; the pool must come from
// pkg/dbconn. A non-unique index is admitted without a read.
func AdmitPartitionedUniqueIndex(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	st, op, err := admitPartitionedIndexShape(sql)
	if err != nil {
		return err
	}
	if !op.Unique {
		return nil
	}
	keyColumns, err := statement.IndexKeyColumns(sql)
	if err != nil {
		return fmt.Errorf("read the key columns of index %s: %w", qualifiedName(st.Schema(), op.Name), err)
	}
	return briefTx{pool: pool}.read(ctx, "read the partition keys", func(ctx context.Context, q querier) error {
		target, err := resolveTarget(ctx, q, concurrentIndexBuild{index: op.Name, tableSchema: st.Schema(), table: st.Table()})
		if err != nil {
			return err
		}
		tree, err := listPartitionTree(ctx, q, target.tableOID)
		if err != nil {
			return err
		}
		return readUncoveredPartitionKeys(ctx, q, target.schema, op.Name, keyColumns, keyedTables(target, tree))
	})
}
