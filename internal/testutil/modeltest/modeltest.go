// Package modeltest compares schemadiff models in tests.
package modeltest

import (
	"slices"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// Comparable returns m without the fields introspection reads on one side
// only, so a live model and the desired model of the same table compare
// equal exactly when they describe the same table. Constraint.IndexDef is
// read for a desired table and never for a live one.
func Comparable(m schemadiff.Model) schemadiff.Model {
	m.Constraints = slices.Clone(m.Constraints)
	for i := range m.Constraints {
		m.Constraints[i].IndexDef = ""
	}
	return m
}
