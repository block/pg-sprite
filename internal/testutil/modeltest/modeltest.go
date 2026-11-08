// Package modeltest compares schemadiff models in tests.
package modeltest

import (
	"slices"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// Comparable returns m without the fields introspection reads on one side
// only, so a live model and the desired model of the same table compare
// equal exactly when they describe the same table. Constraint.IndexDef and
// the replayed texts of definitions are read for a desired table and never
// for a live one.
func Comparable(m schemadiff.Model) schemadiff.Model {
	m.Columns = slices.Clone(m.Columns)
	for i := range m.Columns {
		m.Columns[i].DefaultReplays = nil
	}
	m.Constraints = slices.Clone(m.Constraints)
	for i := range m.Constraints {
		m.Constraints[i].IndexDef = ""
		m.Constraints[i].DefReplays = nil
	}
	m.Indexes = slices.Clone(m.Indexes)
	for i := range m.Indexes {
		m.Indexes[i].DefReplays = nil
	}
	return m
}
