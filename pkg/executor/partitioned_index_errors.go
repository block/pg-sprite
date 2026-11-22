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
	// ErrPartitionedIndexTablespace is returned when the statement has a
	// TABLESPACE clause. A partition's index is built from the definition the
	// server prints for the parent's index, and that definition never names a
	// tablespace, so the clause would place the parent's index and leave every
	// partition's index in the default tablespace. The build refuses the
	// statement before it creates anything.
	ErrPartitionedIndexTablespace = errors.New("a partitioned index build does not support a TABLESPACE clause")
	// ErrPartitionedIndexUnsettled is returned when partitions kept appearing
	// without an attached index for as long as the build's bounded number of
	// passes. What the build finished stays attached.
	ErrPartitionedIndexUnsettled = errors.New("partitions kept appearing without an index until the build ran out of passes")
)
