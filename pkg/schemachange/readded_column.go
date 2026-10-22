package schemachange

import "github.com/block/pg-sprite/pkg/schemadiff"

// refuseUnfillableReadds refuses a gated statement that drops a column and
// adds one under the same name when the copy cannot fill the new column.
// A re-added column is a new column: the copy leaves it out and every row
// the copy writes takes the column's own default, as under a direct ALTER
// TABLE. That has nowhere to go for three columns, each refused here,
// before the build commits, instead of after the shadow exists:
//
//   - the primary key, which every chunk range, watermark, and streamed
//     change is keyed on, so a copy without it cannot run;
//   - a NOT NULL column with no default, which the first copied row would
//     violate on the server;
//   - the replacement of a source identity column, whose sequence the
//     handoff carries to the same column on the shadow and cannot carry to
//     a new one.
//
// A re-added column the copy can leave to its default — nullable, with a
// default, a generated column, or an identity of its own replacing a plain
// column — is not refused. Both position lists are in attribute order, as
// readColumnPositions returns them; pk is the source's primary key column
// and identities are its identity columns.
func refuseUnfillableReadds(schema, table, pk string, sourcePositions, shadowPositions []columnPosition, shadow schemadiff.Model, identities []IdentityColumn) error {
	same := sameColumns(sourcePositions, shadowPositions)
	shadowColumns := make(map[string]schemadiff.Column, len(shadow.Columns))
	for _, c := range shadow.Columns {
		shadowColumns[c.Name] = c
	}
	sourceIdentity := make(map[string]bool, len(identities))
	for _, id := range identities {
		sourceIdentity[id.Column] = true
	}
	for _, source := range sourcePositions {
		column, readded := shadowColumns[source.name]
		if same[source.name] || !readded {
			continue
		}
		// INV: ST-11
		if source.name == pk {
			return refuse(CauseStatementReadd, nil, "the statement for %s.%s drops and re-adds the primary key %s, which the copy is keyed on", schema, table, source.name)
		}
		if sourceIdentity[source.name] {
			return refuse(CauseStatementReadd, nil, "the statement for %s.%s drops and re-adds identity column %s, whose sequence cannot be handed to a new column", schema, table, source.name)
		}
		if requiresAValueTheCopyCannotGive(column) {
			return refuse(CauseStatementReadd, nil, "the statement for %s.%s drops and re-adds column %s NOT NULL with no default, so the copy has no value to give it", schema, table, source.name)
		}
	}
	return nil
}

// requiresAValueTheCopyCannotGive reports whether a new column the copy
// leaves out would reject every copied row: it refuses NULL and the server
// has nothing to fill it with — no default, no identity, no generation
// expression.
func requiresAValueTheCopyCannotGive(c schemadiff.Column) bool {
	if !c.NotNull {
		return false
	}
	if c.Default != "" || c.IsGenerated() {
		return false
	}
	return c.Identity == schemadiff.IdentityNone
}
