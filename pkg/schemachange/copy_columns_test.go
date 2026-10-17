package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// The copy lists only columns that store data and survive on the shadow:
// a generated column of either kind is recomputed there, never copied, and
// a dropped column has nowhere to go.
func TestCopyColumnsSkipsGeneratedAndDroppedColumns(t *testing.T) {
	source := schemadiff.Model{Columns: []schemadiff.Column{
		{Name: "id", Type: "bigint"},
		{Name: "n", Type: "integer"},
		{Name: "stored", Type: "integer", Generated: true, Default: "(n * 2)"},
		{Name: "virtual", Type: "integer", Virtual: true, Default: "(n * 3)"},
		{Name: "legacy", Type: "text"},
	}}
	same := map[string]bool{"id": true, "n": true, "stored": true, "virtual": true}
	assert.Equal(t, []string{"id", "n"}, copyColumns(source, same))
}

// A column is the same column on the shadow when its attribute number there
// is its live position on the source: the shadow is numbered 1..n in source
// order, a retype keeps the number, and an added column is numbered past n.
// The source's own numbers have a gap where it once dropped a column, so its
// position is its index.
func TestSameColumnsComparesLivePositionToShadowNumber(t *testing.T) {
	source := []columnPosition{
		{name: "id", attnum: 1},
		{name: "label", attnum: 3},
		{name: "qty", attnum: 4},
	}
	shadow := []columnPosition{
		{name: "id", attnum: 1},
		{name: "qty", attnum: 3},
		{name: "label", attnum: 4},
	}
	assert.Equal(t, map[string]bool{"id": true, "qty": true}, sameColumns(source, shadow),
		"label sits at number 4 on the shadow, past the 3 columns it was cloned with")
}

// A column the change dropped has no counterpart on the shadow at all.
func TestSameColumnsOmitsADroppedColumn(t *testing.T) {
	source := []columnPosition{{name: "id", attnum: 1}, {name: "note", attnum: 2}}
	shadow := []columnPosition{{name: "id", attnum: 1}}
	assert.Equal(t, map[string]bool{"id": true}, sameColumns(source, shadow))
}
