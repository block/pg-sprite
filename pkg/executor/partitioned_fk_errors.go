package executor

import "errors"

// Typed refusals for giving a leaf partition the foreign key a partitioned
// parent's FOREIGN KEY adopts. Each says what the catalog decided, so a
// caller branches on the cause, not on text. The step never drops or renames
// a constraint it did not create, so each one leaves the leaf as it found it.
var (
	// ErrPartitionedForeignKeyNameTaken is returned when the name the step
	// derives for a leaf's foreign key is held by a constraint of the leaf
	// that is not the requested foreign key.
	ErrPartitionedForeignKeyNameTaken = errors.New("the name the foreign key needs on a partition is held by a constraint that is not the requested foreign key")
	// ErrPartitionedForeignKeyConflict is returned when the leaf already
	// carries a foreign key that keeps the parent's foreign key from
	// adopting the one the step proved: two or more of the requested
	// definition, so which one the parent adopts would be left to the
	// server; one of the requested definition already attached to a parent
	// table's foreign key; or one the parent's statement could adopt whose
	// ON DELETE column list is not the requested one, which the server does
	// not compare when it chooses what to adopt.
	ErrPartitionedForeignKeyConflict = errors.New("the partition already has a foreign key that keeps the parent's from adopting the requested one as the only one")
	// ErrPartitionedForeignKeyReferencedTable is returned when the foreign
	// key references a table the build does not take: anything but a plain
	// table, such as a partitioned table, a foreign table, or a view, or a
	// partition of the table the foreign key is added to.
	ErrPartitionedForeignKeyReferencedTable = errors.New("the foreign key references a table the partitioned foreign key build does not take")
	// ErrPartitionedForeignKeyViolation is returned when a leaf's rows fail
	// the VALIDATE of its foreign key: the rows need correcting before a
	// rerun. The foreign key stays NOT VALID on the leaf, still enforced for
	// new rows, and a rerun validates it where it stands. The server's error
	// stays in the chain for logs; its Detail names the offending key value,
	// so it is not for a shared surface.
	ErrPartitionedForeignKeyViolation = errors.New("rows of the partition violate the foreign key")
)

// sqlstateForeignKeyViolation is the server's foreign_key_violation.
const sqlstateForeignKeyViolation = "23503"
