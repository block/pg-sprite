package schemadiff

import "strings"

// DroppedTableDefinition renders the definition a reviewed DROP TABLE
// carries: everything the drop removes and everything that ties the table
// to others, one fact per line in a stable order. It is read from a model
// introspection produced, so the definition a plan records and the one
// execution reads again under the table's lock print alike, and any change
// to the table between the two shows as a difference.
//
// The table's own columns, constraints, indexes, and the objects that go
// with each column are listed, because the drop removes them. Partitioning,
// inheritance, and the foreign keys of other tables that reference it are
// listed, because each changes what the drop would reach or whether the
// server allows it. Rows are not part of the definition: a table's data
// changes all the time and its loss is what the reviewer accepted.
func DroppedTableDefinition(m Model) string {
	var b strings.Builder
	line := func(parts ...string) {
		b.WriteString(strings.Join(parts, ""))
		b.WriteByte('\n')
	}
	line("table ", m.Table)
	if m.Unlogged {
		line("unlogged")
	}
	if m.IsPartition {
		line("partition of a partitioned table")
	}
	if m.PartitionKey != "" {
		line("partition by ", m.PartitionKey)
	}
	for _, parent := range m.InheritsParents {
		line("inherits ", parent)
	}
	for _, child := range m.InheritanceChildren {
		line("inherited by ", child)
	}
	for _, opt := range m.Options {
		line("option ", opt)
	}
	for _, c := range m.Columns {
		line("column ", c.Name, ": ", c.DroppedDefinition())
		for _, dep := range DependentLines(c.Dependents) {
			line("  with ", dep)
		}
	}
	for _, con := range m.Constraints {
		line("constraint ", con.Name, ": ", con.Def)
	}
	for _, ix := range m.Indexes {
		line("index ", ix.Name, ": ", ix.Def)
	}
	for _, ref := range m.ReferencedBy {
		line("referenced by ", ref)
	}
	return b.String()
}
