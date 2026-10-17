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
	target := schemadiff.Model{Columns: []schemadiff.Column{
		{Name: "id", Type: "bigint"},
		{Name: "n", Type: "integer"},
		{Name: "stored", Type: "integer", Generated: true, Default: "(n * 2)"},
		{Name: "virtual", Type: "integer", Virtual: true, Default: "(n * 3)"},
	}}
	assert.Equal(t, []string{"id", "n"}, copyColumns(source, target))
}
