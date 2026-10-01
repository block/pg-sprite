package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Pairing is by definition, never by name: a source index finds the shadow
// index with the same key whatever either is called, and the pair order
// follows the source. A source dependent without a partner and a shadow
// dependent without one are both reported, not refused — the gated
// statement may have dropped an index's column or added an index.
func TestPairByDefinitionPairsEqualKeysAndReportsTheRest(t *testing.T) {
	source := []dependent{
		{name: "orders_pkey", key: "pk(id)"},
		{name: "orders_note_idx", key: "btree(note)"},
		{name: "orders_qty_idx", key: "btree(qty)"},
	}
	shadow := []dependent{
		{name: "_pgsprite_x_new_qty_idx", key: "btree(qty)"},
		{name: "_pgsprite_x_new_pkey", key: "pk(id)"},
		{name: "qty_positive_idx", key: "btree(qty) WHERE qty > 0"},
	}

	got := pairByDefinition(DependentIndex, source, shadow)

	assert.Equal(t, DependentPairing{
		Pairs: []DependentPair{
			{Kind: DependentIndex, SourceName: "orders_pkey", ShadowName: "_pgsprite_x_new_pkey"},
			{Kind: DependentIndex, SourceName: "orders_qty_idx", ShadowName: "_pgsprite_x_new_qty_idx"},
		},
		UnpairedSource: []string{"orders_note_idx"},
		UnpairedShadow: []string{"qty_positive_idx"},
	}, got)
}

// Two source dependents with one definition are interchangeable (D8): each
// takes a distinct shadow partner, and a third with the same definition on
// the source side is left unpaired rather than sharing a partner, since a
// shared partner would be renamed twice.
func TestPairByDefinitionNeverReusesAShadowPartner(t *testing.T) {
	source := []dependent{
		{name: "dup_a", key: "btree(qty)"},
		{name: "dup_b", key: "btree(qty)"},
		{name: "dup_c", key: "btree(qty)"},
	}
	shadow := []dependent{
		{name: "shadow_qty_idx", key: "btree(qty)"},
		{name: "shadow_qty_idx1", key: "btree(qty)"},
	}

	got := pairByDefinition(DependentStatistics, source, shadow)

	assert.Equal(t, []DependentPair{
		{Kind: DependentStatistics, SourceName: "dup_a", ShadowName: "shadow_qty_idx"},
		{Kind: DependentStatistics, SourceName: "dup_b", ShadowName: "shadow_qty_idx1"},
	}, got.Pairs)
	assert.Equal(t, []string{"dup_c"}, got.UnpairedSource)
	assert.Empty(t, got.UnpairedShadow)
}

// The derived names cutover must find free are the retained table's and one
// per source-side dependent — paired or not — plus whatever the caller adds
// (identity sequences); unpaired shadow dependents keep their names and
// need none.
func TestOldNamesCoverTheTableAndEverySourceDependent(t *testing.T) {
	pairing := DependentPairing{
		Pairs:          []DependentPair{{Kind: DependentIndex, SourceName: "orders_pkey", ShadowName: "s_pkey"}},
		UnpairedSource: []string{"orders_note_idx"},
		UnpairedShadow: []string{"qty_positive_idx"},
	}

	got := oldNames("sales", "orders", pairing.sourceNames())

	assert.Equal(t, []string{
		OldName("sales", "orders"),
		OldDependentName("sales", "orders", "orders_pkey"),
		OldDependentName("sales", "orders", "orders_note_idx"),
	}, got)
}
