package schemadiff

import (
	"strconv"
	"strings"
)

// DroppedTableDefinition renders the definition a reviewed DROP TABLE
// carries: everything the drop removes and everything that ties the table
// to others, one fact per line in a stable order. It is read from a model
// introspection produced, so the definition a plan records and the one
// execution reads again under the table's lock print alike, and any change
// to the table between the two shows as a difference.
//
// The table's own columns, constraints, indexes, row security settings and
// policies, the objects that go with each column, and the objects that go
// with the whole table are listed, because the drop removes them.
// Partitioning, inheritance, and the foreign keys of other tables that
// reference it are listed, because each changes what the drop would reach
// or whether the server allows it. Each incoming foreign key is one
// "referenced by" line, named as [Model.ReferencedBy] names it, so a caller
// that drops the referencing table first records the definition without
// that line. Rows are not part of the definition: a table's data changes
// all the time and its loss is what the reviewer accepted.
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
		for _, dep := range c.Dependents {
			// An incoming foreign key is listed once, by the table that owns
			// it, below: the drop does not remove it, and a caller that drops
			// the referencing table first removes that one entry.
			if dep.Referencing {
				continue
			}
			line("  with ", dep.String())
		}
	}
	for _, con := range m.Constraints {
		line("constraint ", con.Name, ": ", con.Def)
	}
	for _, ix := range m.Indexes {
		line("index ", ix.Name, ": ", ix.Def)
	}
	for _, dep := range m.TableDependents {
		if dep.Def == "" {
			line("with ", dep.Name)
		} else {
			line("with ", dep.Name, ": ", dep.Def)
		}
	}
	if m.RowSecurity.Enabled {
		line("row security enabled")
	}
	if m.RowSecurity.Forced {
		line("row security forced")
	}
	for _, p := range m.RowSecurity.Policies {
		line("policy ", policyDefinition(p))
	}
	for _, ref := range m.ReferencedBy {
		line("referenced by ", ref)
	}
	return b.String()
}

// policyDefinition prints every field of a policy on one line, so a policy
// that changed in any way prints differently.
func policyDefinition(p Policy) string {
	optional := func(v *string) string {
		if v == nil {
			return "none"
		}
		return strconv.Quote(*v)
	}
	mode := "restrictive"
	if p.Permissive {
		mode = "permissive"
	}
	return p.Name + ": " + mode + " for " + string(p.Command) + " to " + strings.Join(p.Roles, ", ") +
		" using " + optional(p.Using) + " with check " + optional(p.WithCheck) + " comment " + optional(p.Comment)
}
