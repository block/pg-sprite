package executor

import "errors"

// Typed refusals for giving a leaf partition the key constraint a partitioned
// parent's PRIMARY KEY or UNIQUE constraint adopts. Each says what the catalog
// decided, so a caller branches on the cause, not on text. The step never
// drops or renames an object it did not create, so each one leaves the leaf
// as it found it.
var (
	// ErrPartitionedKeyNameTaken is returned when the name the step derives
	// for a leaf's index and constraint is held by something that is not the
	// requested key: another kind of relation, an index of another
	// definition or on another table, an index already attached to a
	// partitioned index or backing a constraint, or a constraint that is not
	// the requested key.
	ErrPartitionedKeyNameTaken = errors.New("the name the key constraint needs on a partition is held by something that is not the requested key")
	// ErrPartitionedKeyConflict is returned when the leaf already carries a
	// key constraint the step cannot reconcile with the requested one: two
	// constraints that each match it, a constraint on the same columns that
	// differs in kind or definition, one already attached to a parent's
	// constraint, or, for a primary key, a primary key on other columns. The
	// parent's constraint could adopt the wrong one, or the server would
	// refuse a second primary key, so the step adds nothing.
	ErrPartitionedKeyConflict = errors.New("the partition already has a key constraint that conflicts with the requested one")
	// ErrPartitionedKeyNullableColumn is returned when a primary key is
	// requested over a column the leaf allows to be NULL. Adding the primary
	// key would set NOT NULL, which scans the whole partition while it holds
	// ACCESS EXCLUSIVE, so the step refuses before it builds anything.
	ErrPartitionedKeyNullableColumn = errors.New("a primary key column of the partition allows NULL, and setting NOT NULL would scan it under ACCESS EXCLUSIVE")
)

// Typed refusals of the partitioned key build as a whole. Each is returned
// before the build creates anything, except ErrPartitionedKeyUnsettled,
// which leaves every partition's finished key constraint in place for a rerun
// to adopt.
var (
	// ErrNotAddKeyConstraint is returned when the statement is not one ALTER
	// TABLE with exactly one ADD PRIMARY KEY or ADD UNIQUE subcommand.
	ErrNotAddKeyConstraint = errors.New("statement is not a single ALTER TABLE ... ADD PRIMARY KEY or ADD UNIQUE")
	// ErrPartitionedKeyUnsupportedShape is returned when the ADD PRIMARY KEY
	// or ADD UNIQUE carries a clause the build cannot carry to every
	// partition, or names no constraint. The build gives each partition an
	// index and a constraint whose names derive from the constraint's name,
	// and a rerun finds them by that name, so an unnamed constraint, whose
	// name the server would choose, could not be resumed. The clauses
	// (INCLUDE, NULLS NOT DISTINCT, DEFERRABLE, INITIALLY DEFERRED, WITH,
	// USING INDEX TABLESPACE, USING INDEX, NOT VALID, ONLY, a repeated
	// column) would make the partitions' constraints differ from the
	// parent's, so the parent could not adopt them, or are not a key build
	// at all.
	ErrPartitionedKeyUnsupportedShape = errors.New("the partitioned key build takes a named PRIMARY KEY or UNIQUE constraint over a plain list of distinct columns, with no other clause")
	// ErrPartitionedKeyParentConflict is returned when the partitioned table
	// already has a constraint under the requested name that is not the
	// requested key, a relation in its schema already holds the name the
	// parent's index would take, or a primary key is requested and the table
	// already has one.
	ErrPartitionedKeyParentConflict = errors.New("the partitioned table already has a constraint, an index, or a primary key that stands in the way of the requested key")
	// ErrPartitionedKeySubPartitioned is returned when a partition of the
	// target is itself partitioned. The build gives the key to leaf
	// partitions directly under the target only.
	ErrPartitionedKeySubPartitioned = errors.New("a partition of the target is itself partitioned, and the partitioned key build takes only leaf partitions directly under the target")
	// ErrPartitionedKeyUnsettled is returned when partitions kept appearing
	// without the key constraint for as long as the build's bounded number of
	// passes. Every partition's finished constraint stays, and a rerun
	// resumes.
	ErrPartitionedKeyUnsettled = errors.New("partitions kept appearing without the key constraint until the build ran out of passes")
)
