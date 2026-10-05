package schemachange

import "encoding/json"

// SwapProof is the plain view of a SwappedTable: the same facts as its
// accessors, as exported fields a checkpoint can encode and decode. A
// resume decodes the checkpoint it kept into a SwapProof and compares it
// with the SwapProof of what InspectSwapped returned. SwapProof is a view
// only: nothing turns it back into a SwappedTable, so DropOldTable still
// accepts only a value the cutover or the inspection minted.
type SwapProof struct {
	// Schema is the schema both tables live in.
	Schema string `json:"schema"`
	// Table is the name the live table bears: the source's name.
	Table string `json:"table"`
	// OldTable is the name the retained source bears.
	OldTable string `json:"old_table"`
	// LiveOID is the live table's OID: the shadow the build created.
	LiveOID uint32 `json:"live_oid"`
	// OldOID is the retained source's OID.
	OldOID uint32 `json:"old_oid"`
	// Owner is the role that owns both tables.
	Owner string `json:"owner"`
	// Indexes are the index renames the swap performed.
	Indexes DependentPairing `json:"indexes"`
	// Statistics are the extended-statistics renames the swap performed.
	Statistics DependentPairing `json:"statistics"`
	// OwnedSequences are the sequences the swap re-owned to the live table.
	OwnedSequences []OwnedSequence `json:"owned_sequences"`
	// IdentityColumns are the identity columns the live table carries.
	IdentityColumns []IdentityColumn `json:"identity_columns"`
	// Attempts is how many ACCESS EXCLUSIVE acquisitions the swap needed;
	// zero in a proof re-derived after the fact.
	Attempts int `json:"attempts"`
}

// Proof returns the plain view of the swapped table: each field is what
// the accessor of the same name returns.
func (s SwappedTable) Proof() SwapProof {
	return SwapProof{
		Schema:          s.Schema(),
		Table:           s.Table(),
		OldTable:        s.OldTable(),
		LiveOID:         s.LiveOID(),
		OldOID:          s.OldOID(),
		Owner:           s.Owner(),
		Indexes:         s.Indexes(),
		Statistics:      s.Statistics(),
		OwnedSequences:  s.OwnedSequences(),
		IdentityColumns: s.IdentityColumns(),
		Attempts:        s.Attempts(),
	}
}

// MarshalJSON encodes the swapped table as its SwapProof, so a checkpoint
// can store a SwappedTable directly. SwappedTable has no UnmarshalJSON on
// purpose: decode into a SwapProof instead.
func (s SwappedTable) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Proof())
}
