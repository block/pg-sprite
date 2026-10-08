package schemachange

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// SwappedTable is the proof that the swap committed: the shadow now bears
// the source's name and the source its derived _old name, with the
// dependents renamed and the sequences handed off as recorded here. Its
// constructor is private: only Cutover and InspectSwapped mint one, and
// DropOldTable accepts nothing else.
type SwappedTable struct {
	schema     string
	table      string
	liveOID    uint32
	oldOID     uint32
	owner      string
	indexes    DependentPairing
	statistics DependentPairing
	sequences  []OwnedSequence
	identities []IdentityColumn
	attempts   int
}

// newSwappedTable records what the swap transaction renamed and handed off.
// The identities are the ones the handoff left on the live table, read back
// from it, not the source's: a widened column carries widened bounds.
func newSwappedTable(ready CutoverReady, identities []IdentityColumn, attempts int) SwappedTable {
	built := ready.built
	return SwappedTable{
		schema:     built.Schema(),
		table:      built.SourceTable(),
		liveOID:    built.ShadowOID(),
		oldOID:     built.SourceOID(),
		owner:      built.Fidelity().Owner,
		indexes:    ready.indexes,
		statistics: ready.statistics,
		sequences:  ready.sequences,
		identities: identities,
		attempts:   attempts,
	}
}

// Schema is the schema both tables live in.
func (s SwappedTable) Schema() string { return s.schema }

// Table is the name the live table bears: the source's name.
func (s SwappedTable) Table() string { return s.table }

// OldTable is the name the retained source bears after the swap.
func (s SwappedTable) OldTable() string { return OldName(s.schema, s.table) }

// LiveOID is the live table's OID: the shadow the build created.
func (s SwappedTable) LiveOID() uint32 { return s.liveOID }

// OldOID is the retained source's OID.
func (s SwappedTable) OldOID() uint32 { return s.oldOID }

// Owner is the role that owns both tables, which the old table's drop
// runs as. It is the role the build recorded; the swap carries it forward
// and the inspection confirms both relations are still owned by it before
// minting, so a proof never names an owner the catalog disagrees with.
func (s SwappedTable) Owner() string { return s.owner }

// Indexes are the index renames the swap performed, paired by definition.
// In a SwappedTable InspectSwapped re-derived, each pair's ShadowName is
// empty and an unpaired source index is listed under the derived name it
// now bears.
func (s SwappedTable) Indexes() DependentPairing { return s.indexes }

// Statistics are the extended-statistics renames the swap performed, with
// the same shape as Indexes when re-derived.
func (s SwappedTable) Statistics() DependentPairing { return s.statistics }

// OwnedSequences are the sequences the swap re-owned to the live table.
func (s SwappedTable) OwnedSequences() []OwnedSequence {
	return append([]OwnedSequence(nil), s.sequences...)
}

// IdentityColumns are the identity columns the swap recreated on the live
// table, each with the options its sequence now declares: the source
// sequence's, with a bound the source took from its column's old type
// moved to the new type when the statement widened the column.
func (s SwappedTable) IdentityColumns() []IdentityColumn {
	return append([]IdentityColumn(nil), s.identities...)
}

// Attempts is how many ACCESS EXCLUSIVE acquisitions the swap needed.
func (s SwappedTable) Attempts() int { return s.attempts }

// confirmSwap re-reads the catalog inside the swap transaction, after the
// renames and handoffs, and refuses to commit unless it shows exactly what
// the swap set out to produce: the shadow's OID under the source name, the
// source's OID under the _old name, every paired shadow dependent under its
// source partner's name, every owned sequence on the live table, and every
// identity column recreated under the source sequence's name with the
// options the handoff set out to declare — computed from the source's and
// the live column's type before the handoff ran, so this read checks the
// handoff's work rather than repeating it.
func confirmSwap(ctx context.Context, tx pgx.Tx, ready CutoverReady, identities []IdentityColumn) error {
	built := ready.built
	schema, live, old := built.Schema(), built.SourceTable(), OldName(built.Schema(), built.SourceTable())
	liveOID, err := resolveRelation(ctx, tx, schema, live)
	if err != nil {
		return err
	}
	// INV: ST-6
	if liveOID != built.ShadowOID() {
		return refuse(CauseSwapMismatch, nil, "after the swap %s.%s is relation %d, not the shadow %d", schema, live, liveOID, built.ShadowOID())
	}
	oldOID, err := resolveRelation(ctx, tx, schema, old)
	if err != nil {
		return err
	}
	if oldOID != built.SourceOID() {
		return refuse(CauseSwapMismatch, nil, "after the swap %s.%s is relation %d, not the source %d", schema, old, oldOID, built.SourceOID())
	}
	indexes, err := readIndexes(ctx, tx, liveOID)
	if err != nil {
		return fmt.Errorf("live %s.%s: %w", schema, live, err)
	}
	if missing := missingNames(ready.indexes.Pairs, indexNames(indexes)); len(missing) > 0 {
		return refuse(CauseSwapMismatch, nil, "after the swap live %s.%s lacks indexes it was to take over: %s", schema, live, joinNames(missing))
	}
	statistics, err := readStatistics(ctx, tx, liveOID)
	if err != nil {
		return fmt.Errorf("live %s.%s: %w", schema, live, err)
	}
	if missing := missingNames(ready.statistics.Pairs, dependentNames(statistics)); len(missing) > 0 {
		return refuse(CauseSwapMismatch, nil, "after the swap live %s.%s lacks statistics it was to take over: %s", schema, live, joinNames(missing))
	}
	sequences, err := readOwnedSequences(ctx, tx, liveOID)
	if err != nil {
		return fmt.Errorf("live %s.%s: %w", schema, live, err)
	}
	if !slices.Equal(sequences, ready.sequences) {
		return refuse(CauseSwapMismatch, nil, "after the swap live %s.%s owns different sequences than the source did", schema, live)
	}
	onLive, err := readIdentityColumns(ctx, tx, liveOID)
	if err != nil {
		return fmt.Errorf("live %s.%s: %w", schema, live, err)
	}
	if !slices.Equal(onLive, identities) {
		return refuse(CauseSwapMismatch, nil, "after the swap live %s.%s identity columns differ from what the handoff declared", schema, live)
	}
	return nil
}

// missingNames lists each pair's source name that the live table does not
// carry.
func missingNames(pairs []DependentPair, live []string) []string {
	var missing []string
	for _, pair := range pairs {
		if !slices.Contains(live, pair.SourceName) {
			missing = append(missing, pair.SourceName)
		}
	}
	return missing
}
