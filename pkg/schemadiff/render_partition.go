package schemadiff

import (
	"errors"
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrUnrenderableUnattachedIndex is returned for a partitioned parent
// carrying an invalid index: one that some partition has no attached index
// for. A desired file declares the parent's index for the whole table, and
// the diff compares a parent's indexes by name and definition alone, so a
// rendered baseline would present the partial index as complete.
var ErrUnrenderableUnattachedIndex = errors.New("a partitioned parent's index that is not attached on every partition cannot be rendered as a desired schema")

// ErrUnrenderablePartitionedRowSecurity is returned when a partitioned
// parent carries row security. Policies on the parent do not apply to a
// query that names a partition directly, and the row security executors
// refuse a partitioned parent, so a rendered file could never be applied.
var ErrUnrenderablePartitionedRowSecurity = errors.New("row security on a partitioned parent cannot be rendered as a desired schema")

// partitionedParentRefusal refuses a partitioned parent whose rendering
// would misstate it. Partitions, their bounds, and the attachments are not
// in the model: a parent renders as its own definition only.
func partitionedParentRefusal(m Model) error {
	for _, ix := range m.Indexes {
		if ix.Invalid {
			return fmt.Errorf("index %q is not attached on every partition: %w", ix.Name, ErrUnrenderableUnattachedIndex)
		}
	}
	for _, c := range m.Constraints {
		if c.IndexInvalid {
			return fmt.Errorf("the index of constraint %q is not attached on every partition: %w", c.Name, ErrUnrenderableUnattachedIndex)
		}
	}
	return nil
}

// renderIndexDef returns the CREATE INDEX statement a desired file declares
// for one of the model's indexes. A partitioned parent's index is declared
// on the whole table, without the ONLY the server prints for it.
func renderIndexDef(m Model, ix Index) (string, error) {
	if m.PartitionKey == "" {
		return ix.Def, nil
	}
	def, err := statement.IndexOnWholeTable(ix.Def)
	if err != nil {
		return "", fmt.Errorf("index %q: %w", ix.Name, err)
	}
	return def, nil
}
