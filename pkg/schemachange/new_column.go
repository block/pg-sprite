package schemachange

import (
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// refuseUnfillableNewColumns refuses a gated statement that leaves the
// shadow with a column the copy cannot fill, or without the column the
// copy is keyed on. The copy list names only the source columns the shadow
// kept as the same column; every other shadow column is a new column the
// copy leaves out, and every row the copy writes takes that column's own
// default, or its domain's, as under a direct ALTER TABLE. Two shapes have
// nowhere to go, and are refused here, before the build commits:
//
//   - the primary key is not among the kept columns — dropped outright, or
//     replaced by a key under a new name — so every chunk range, watermark,
//     and streamed change names a column the shadow does not have;
//   - a new column that refuses NULL, by its own NOT NULL or its domain's,
//     with nothing the server fills it from, which the first copied row
//     would violate.
//
// A new column the copy can leave to the server — nullable, with a default
// of its own or its domain's, or generated — is not refused here. A column
// dropped and added again under its own name is a new column too, and is
// refused before this check as the re-add it is; a new identity or serial
// column is fillable by the server and is refused after it as a sequence
// the swap cannot carry. Both position lists are in attribute order, as
// readColumnPositions returns them; pk is the source's primary key column
// and domains is the shadow's readDomainFills.
func refuseUnfillableNewColumns(schema, table, pk string, sourcePositions, shadowPositions []columnPosition, shadow schemadiff.Model, domains map[string]domainFill) error {
	same := sameColumns(sourcePositions, shadowPositions)
	// INV: ST-11
	if !same[pk] {
		return refuse(CauseStatementNewColumn, nil, "the statement for %s.%s leaves the table without its primary key %s, which the copy is keyed on; a key under another name does not lift this, so change the key in place with ALTER COLUMN TYPE instead", schema, table, pk)
	}
	for _, column := range shadow.Columns {
		if same[column.Name] {
			continue
		}
		if requiresAValueTheCopyCannotGive(column, domains[column.Name]) {
			return refuse(CauseStatementNewColumn, nil, "the statement for %s.%s adds column %s, which refuses NULL and has no default of its own or from its type, so the copy has no value to give it; give the new column a default, or add it in a later schema change the server applies in place", schema, table, column.Name)
		}
	}
	return nil
}
