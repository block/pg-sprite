package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// Nothing is true only when no leftover of any kind is named: the table's
// names alone are nothing, and each kind of leftover on its own is
// something.
func TestDiscardedNothingCountsEveryKindOfLeftover(t *testing.T) {
	names := schemachange.Discarded{Schema: "app", Table: "orders"}
	assert.True(t, names.Nothing())

	for kind, discarded := range map[string]schemachange.Discarded{
		"row phase":   {Schema: "app", Table: "orders", RowPhase: checkpoint.PhaseCopying},
		"shadow":      {Schema: "app", Table: "orders", Shadow: "_pgsprite_0123456789abcdef_new"},
		"slot":        {Schema: "app", Table: "orders", Slot: "pgsprite_01234567"},
		"publication": {Schema: "app", Table: "orders", Publication: "pgsprite_01234567"},
		"write fence": {Schema: "app", Table: "orders", WriteFence: true},
	} {
		assert.False(t, discarded.Nothing(), kind)
	}
}
