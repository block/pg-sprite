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
	// differs in kind or definition or is DEFERRABLE, one already attached to
	// a parent's constraint, or, for a primary key, a primary key on other
	// columns. The parent's constraint could adopt the wrong one, or the
	// server would refuse a second primary key, so the step adds nothing.
	ErrPartitionedKeyConflict = errors.New("the partition already has a key constraint that conflicts with the requested one")
	// ErrPartitionedKeyNullableColumn is returned when a primary key is
	// requested over a column the leaf allows to be NULL, including one whose
	// not-null constraint is NOT VALID (PostgreSQL 18). Adding the primary
	// key would set NOT NULL, which scans the whole partition while it holds
	// ACCESS EXCLUSIVE, so the step refuses before it builds anything.
	ErrPartitionedKeyNullableColumn = errors.New("a primary key column of the partition allows NULL, and setting NOT NULL would scan it under ACCESS EXCLUSIVE")
)
