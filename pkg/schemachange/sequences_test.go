package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// The sequences the swap re-owns are those of columns the shadow kept, in
// the source's order; a sequence owned by a column the statement dropped
// has no column on the shadow to be re-owned to and stays with the old
// table (D5). A statement that drops no column keeps every sequence.
func TestKeptSequencesFollowTheShadowsColumns(t *testing.T) {
	sequences := []OwnedSequence{
		{Column: "id", SequenceSchema: "sales", SequenceName: "orders_id_seq"},
		{Column: "legacy_id", SequenceSchema: "sales", SequenceName: "orders_legacy_id_seq"},
		{Column: "batch", SequenceSchema: "sales", SequenceName: "orders_batch_seq"},
	}
	withoutLegacy := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "qty"}, {Name: "batch"}}}
	assert.Equal(t, []OwnedSequence{sequences[0], sequences[2]}, keptSequences(sequences, withoutLegacy))

	all := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "legacy_id"}, {Name: "batch"}, {Name: "qty"}}}
	assert.Equal(t, sequences, keptSequences(sequences, all))

	assert.Empty(t, keptSequences(nil, all))
}
