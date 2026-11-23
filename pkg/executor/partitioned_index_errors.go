package executor

import "errors"

// Typed refusals for the partitioned index build. Each says what the catalog
// or the statement decided, so a caller branches on the cause, not on text.
var (
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
)
