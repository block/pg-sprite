package executor

import "errors"

// Typed refusals for the partitioned index build. Each says what the catalog
// or the statement decided, so a caller branches on the cause, not on text.
var (
	// ErrNotCreateIndex is returned when the statement is not a single
	// CREATE INDEX.
	ErrNotCreateIndex = errors.New("statement is not a CREATE INDEX")
	// ErrNotPartitionedTable is returned when the target is not a partitioned
	// table, at admission or when the catalog is read under the table lock. A
	// plain table's index is the concurrent build's job.
	ErrNotPartitionedTable = errors.New("the target is not a partitioned table")
	// ErrPartitionedIndexNameTaken is returned when a name the build needs is
	// held by a relation that is not the requested index: another kind of
	// relation, an index of another definition, or an index on another table.
	// The build never chooses another name and never drops what it did not
	// create, so the occupant is left for an operator.
	ErrPartitionedIndexNameTaken = errors.New("an index name the build needs is held by a relation that is not the requested index")
	// ErrForeignTablePartition is returned when the partition tree holds a
	// foreign table. It cannot carry an index, so the parent's index would
	// never become valid; the build refuses the whole tree before it creates
	// anything.
	ErrForeignTablePartition = errors.New("the partition tree holds a foreign table, which cannot carry an index")
	// ErrUniqueIndexPartitionKey is returned for a unique index that does not
	// contain the partition key of every partitioned table in the tree as
	// plain key columns. PostgreSQL checks each partitioned table on its
	// own, so the build refuses the whole tree before it creates anything
	// rather than fail at the first level that does not match.
	ErrUniqueIndexPartitionKey = errors.New("a unique index must contain the partition key of every partitioned table in the tree")
	// ErrTableLockUnconfirmed is returned when the pool's role cannot see
	// when the session holding the build's table lock started. The build
	// identifies that session by its PID and start time, so it refuses
	// before any step rather than accept a PID that another server could
	// share. Nothing was created.
	ErrTableLockUnconfirmed = errors.New("the pool cannot confirm that the build's own session holds the table lock")
	// ErrPartitionedIndexUnsettled is returned when partitions kept appearing
	// without an attached index for as long as the build's bounded number of
	// passes. What the build finished stays attached.
	ErrPartitionedIndexUnsettled = errors.New("partitions kept appearing without an index until the build ran out of passes")
)
