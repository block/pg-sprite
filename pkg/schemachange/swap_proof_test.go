package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// cutoverProof is the SwapProof a cutover returns for a table with one
// paired index, one re-owned sequence, and one identity column.
func cutoverProof() schemachange.SwapProof {
	return schemachange.SwapProof{
		Schema:   "app",
		Table:    "orders",
		OldTable: schemachange.OldName("app", "orders"),
		LiveOID:  2001,
		OldOID:   1001,
		Owner:    "app_owner",
		Indexes: schemachange.DependentPairing{
			Pairs:          []schemachange.DependentPair{{Kind: schemachange.DependentIndex, SourceName: "orders_pkey", ShadowName: "orders_shadow_pkey"}},
			UnpairedSource: []string{"orders_note_idx"},
			UnpairedShadow: []string{},
		},
		Statistics:     schemachange.DependentPairing{Pairs: []schemachange.DependentPair{}, UnpairedSource: []string{}, UnpairedShadow: []string{}},
		OwnedSequences: []schemachange.OwnedSequence{{Column: "seqno", SequenceSchema: "app", SequenceName: "orders_seqno_seq"}},
		IdentityColumns: []schemachange.IdentityColumn{{
			Column: "id", Always: true, SequenceSchema: "app", SequenceName: "orders_id_seq",
			Options: schemachange.SequenceOptions{Start: 1, Increment: 1, Min: 1, Max: 9223372036854775807, Cache: 1},
		}},
		Attempts: 2,
	}
}

// A proof re-derived after the fact differs from the cutover's in exactly
// the fields no later read can know — the shadow names in the pairing, the
// derived name an unpaired source dependent now bears, and the attempt
// count — and SameSwap treats the two as the same swap, where == would
// not.
func TestSameSwapIgnoresWhatARederivedProofCannotKnow(t *testing.T) {
	checkpointed := cutoverProof()
	inspected := cutoverProof()
	inspected.Indexes = schemachange.DependentPairing{
		Pairs:          []schemachange.DependentPair{{Kind: schemachange.DependentIndex, SourceName: "orders_pkey"}},
		UnpairedSource: []string{schemachange.OldDependentName("app", "orders", "orders_note_idx")},
		UnpairedShadow: []string{},
	}
	inspected.Attempts = 0

	assert.NotEqual(t, checkpointed, inspected, "the two shapes differ field for field")
	assert.True(t, checkpointed.SameSwap(inspected))
	assert.True(t, inspected.SameSwap(checkpointed), "the comparison is symmetric")
}

// Each fact that identifies what the swap left, changed on its own, makes
// SameSwap false: another relation under either name, another owner,
// another sequence re-owned, or an identity drawing from another sequence
// describe a different swap, whatever the pairing says.
func TestSameSwapIsFalseWhenAnIdentifyingFactDiffers(t *testing.T) {
	differing := map[string]func(p *schemachange.SwapProof){
		"live OID":        func(p *schemachange.SwapProof) { p.LiveOID = 2002 },
		"old OID":         func(p *schemachange.SwapProof) { p.OldOID = 1002 },
		"schema":          func(p *schemachange.SwapProof) { p.Schema = "other" },
		"table":           func(p *schemachange.SwapProof) { p.Table = "invoices" },
		"old table":       func(p *schemachange.SwapProof) { p.OldTable = "orders_retained" },
		"owner":           func(p *schemachange.SwapProof) { p.Owner = "someone_else" },
		"owned sequence":  func(p *schemachange.SwapProof) { p.OwnedSequences[0].SequenceName = "orders_other_seq" },
		"identity column": func(p *schemachange.SwapProof) { p.IdentityColumns[0].Always = false },
	}
	for name, change := range differing {
		t.Run(name, func(t *testing.T) {
			other := cutoverProof()
			change(&other)
			assert.False(t, cutoverProof().SameSwap(other))
		})
	}
}
