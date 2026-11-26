package modeltest

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// PlannedDrop derives, the way a plan does, the definition a drop of the
// named object carries: the live table is diffed against itself without the
// object, and the drop the diff plans for it is returned.
func PlannedDrop(t *testing.T, pool *pgxpool.Pool, schema, table string, kind schemadiff.DroppedKind, name string) schemadiff.DroppedObject {
	t.Helper()
	live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
	require.NoError(t, err)
	desired := live
	desired.Columns = slices.Clone(live.Columns)
	desired.Constraints = slices.Clone(live.Constraints)
	desired.Indexes = slices.Clone(live.Indexes)
	switch kind {
	case schemadiff.DroppedColumn:
		desired.Columns = slices.DeleteFunc(desired.Columns, func(c schemadiff.Column) bool { return c.Name == name })
	case schemadiff.DroppedConstraint:
		desired.Constraints = slices.DeleteFunc(desired.Constraints, func(c schemadiff.Constraint) bool { return c.Name == name })
	case schemadiff.DroppedIndex:
		desired.Indexes = slices.DeleteFunc(desired.Indexes, func(i schemadiff.Index) bool { return i.Name == name })
	case schemadiff.DroppedNotNull:
		for i := range desired.Columns {
			if desired.Columns[i].Name == name {
				desired.Columns[i].NotNull = false
			}
		}
	}
	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	for _, change := range changes {
		if change.Drops != nil && change.Drops.Kind == kind && change.Drops.Name == name {
			return *change.Drops
		}
	}
	require.Failf(t, "no drop planned", "the diff plans no drop of %s %s: %+v", kind, name, changes)
	return schemadiff.DroppedObject{}
}
