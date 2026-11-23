package statement

import (
	"errors"
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// Typed refusals for the partitioned index edits. Both edits parse one
// CREATE INDEX, change a few named fields, and deparse.
var (
	// ErrNotCreateIndex is returned when the statement is not a single
	// CREATE INDEX.
	ErrNotCreateIndex = errors.New("statement is not CREATE INDEX")
	// ErrIndexIfNotExists is returned for CREATE INDEX IF NOT EXISTS: the
	// clause makes the statement a no-op while any relation owns the name,
	// so an edit of it would not say what the edited statement did.
	ErrIndexIfNotExists = errors.New("CREATE INDEX IF NOT EXISTS cannot be edited")
	// ErrIndexUnnamed is returned for a CREATE INDEX with no name: a derived
	// index needs a name a rerun can find.
	ErrIndexUnnamed = errors.New("CREATE INDEX must name its index")
	// ErrUnknownPartitionIndexForm is returned for a PartitionIndexForm the
	// package does not define.
	ErrUnknownPartitionIndexForm = errors.New("unknown partition index form")
)

// PartitionIndexForm is the statement shape PartitionIndex produces.
type PartitionIndexForm int

const (
	// PartitionIndexConcurrently is CREATE INDEX CONCURRENTLY on a plain
	// table: the form for a leaf partition.
	PartitionIndexConcurrently PartitionIndexForm = iota + 1
	// PartitionIndexOnly is CREATE INDEX ... ON ONLY, not concurrent: the
	// form for a partition that is itself partitioned. It builds nothing.
	PartitionIndexOnly
)

// OnlyIndex returns sql, a single named CREATE INDEX, as CREATE INDEX ...
// ON ONLY without CONCURRENTLY: the statement that creates a partitioned
// table's index, marked invalid, without building anything. The server
// rejects CONCURRENTLY on a partitioned table, so the flag is cleared. Nothing
// else changes. This and PartitionIndex are the second executable-SQL AST
// edit pg-sprite permits, after Qualify.
func OnlyIndex(sql string) (string, error) {
	idx, node, err := parseCreateIndex(sql)
	if err != nil {
		return "", err
	}
	idx.Relation.Inh = false
	idx.Concurrent = false
	return deparseOne(node)
}

// PartitionIndex returns parentIndexDef, a partitioned index's definition as
// pg_get_indexdef prints it, as a statement that creates the matching index
// named name on the partition schema.table, in the given form. It sets the
// name, the target relation, and the ONLY and CONCURRENTLY flags; the key,
// predicate, uniqueness, and options stay as the server printed them, which is
// what makes the result attachable to the parent's index.
func PartitionIndex(parentIndexDef, schema, table, name string, form PartitionIndexForm) (string, error) {
	if form != PartitionIndexConcurrently && form != PartitionIndexOnly {
		return "", fmt.Errorf("%w: %d", ErrUnknownPartitionIndexForm, form)
	}
	idx, node, err := parseCreateIndex(parentIndexDef)
	if err != nil {
		return "", err
	}
	idx.Idxname = name
	idx.Relation.Schemaname = schema
	idx.Relation.Relname = table
	idx.Relation.Inh = form == PartitionIndexConcurrently
	idx.Concurrent = form == PartitionIndexConcurrently
	return deparseOne(node)
}

// IndexKeyColumns returns the key columns of sql, a single named CREATE
// INDEX, in key order: the columns its key names as plain columns. An
// expression key and an INCLUDE column are not key columns, so neither is
// listed.
func IndexKeyColumns(sql string) ([]string, error) {
	idx, _, err := parseCreateIndex(sql)
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, p := range idx.GetIndexParams() {
		if name := p.GetIndexElem().GetName(); name != "" {
			cols = append(cols, name)
		}
	}
	return cols, nil
}

// parseCreateIndex parses sql as one named CREATE INDEX without IF NOT
// EXISTS and returns its index node and root node for in-place editing.
func parseCreateIndex(sql string) (*pganalyze.IndexStmt, *pganalyze.Node, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return nil, nil, err
	}
	idx := node.GetIndexStmt()
	switch {
	case idx == nil || idx.GetRelation() == nil:
		return nil, nil, ErrNotCreateIndex
	case idx.GetIfNotExists():
		return nil, nil, ErrIndexIfNotExists
	case idx.GetIdxname() == "":
		return nil, nil, ErrIndexUnnamed
	}
	return idx, node, nil
}
