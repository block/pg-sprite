package schemachange

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SwappedTable is the proof that the swap committed: the shadow now bears
// the source's name and the source its derived _old name, with the
// dependents renamed and the sequences handed off as recorded here. Its
// constructor is private: only Cutover mints one, and DropOldTable
// accepts nothing else.
type SwappedTable struct {
	built      BuiltShadow
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
	return SwappedTable{
		built:      ready.built,
		indexes:    ready.indexes,
		statistics: ready.statistics,
		sequences:  ready.sequences,
		identities: identities,
		attempts:   attempts,
	}
}

// Schema is the schema both tables live in.
func (s SwappedTable) Schema() string { return s.built.Schema() }

// Table is the name the live table bears: the source's name.
func (s SwappedTable) Table() string { return s.built.SourceTable() }

// OldTable is the name the retained source bears after the swap.
func (s SwappedTable) OldTable() string { return OldName(s.built.Schema(), s.built.SourceTable()) }

// LiveOID is the live table's OID: the shadow the build created.
func (s SwappedTable) LiveOID() uint32 { return s.built.ShadowOID() }

// OldOID is the retained source's OID.
func (s SwappedTable) OldOID() uint32 { return s.built.SourceOID() }

// Indexes are the index renames the swap performed, paired by definition.
func (s SwappedTable) Indexes() DependentPairing { return s.indexes }

// Statistics are the extended-statistics renames the swap performed.
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

// swapOutcome is what the catalog says about a swap attempt that did not
// return cleanly.
type swapOutcome int

const (
	// outcomeNotSwapped means the source still bears its name: the attempt
	// rolled back.
	outcomeNotSwapped swapOutcome = iota
	// outcomeSwapped means the shadow bears the source's name: the attempt
	// committed and only the client's knowledge of it was lost.
	outcomeSwapped
	// outcomeAmbiguous means neither relation bears the source's name.
	outcomeAmbiguous
)

// inspectOutcome reads, from a fresh connection, which relation bears the
// source name after a failed attempt (LK-4). The OIDs, not the names,
// decide: the build proved both.
func inspectOutcome(ctx context.Context, pool *pgxpool.Pool, built BuiltShadow) (swapOutcome, error) {
	var oid uint32
	err := pool.QueryRow(ctx, `
		SELECT c.oid
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, built.Schema(), built.SourceTable()).Scan(&oid)
	if errors.Is(err, pgx.ErrNoRows) {
		return outcomeAmbiguous, nil
	}
	if err != nil {
		return outcomeNotSwapped, fmt.Errorf("inspect cutover outcome for %s.%s: %w", built.Schema(), built.SourceTable(), err)
	}
	switch oid {
	case built.SourceOID():
		return outcomeNotSwapped, nil
	case built.ShadowOID():
		return outcomeSwapped, nil
	default:
		return outcomeAmbiguous, nil
	}
}

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
	liveIndexNames := make([]string, 0, len(indexes))
	for _, e := range indexes {
		liveIndexNames = append(liveIndexNames, e.name)
	}
	if missing := missingNames(ready.indexes.Pairs, liveIndexNames); len(missing) > 0 {
		return refuse(CauseSwapMismatch, nil, "after the swap live %s.%s lacks indexes it was to take over: %s", schema, live, joinNames(missing))
	}
	statistics, err := readStatistics(ctx, tx, liveOID)
	if err != nil {
		return fmt.Errorf("live %s.%s: %w", schema, live, err)
	}
	liveStatisticNames := make([]string, 0, len(statistics))
	for _, d := range statistics {
		liveStatisticNames = append(liveStatisticNames, d.name)
	}
	if missing := missingNames(ready.statistics.Pairs, liveStatisticNames); len(missing) > 0 {
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

// readLiveIdentities reads the identity columns the live table carries,
// from a fresh connection, for a swap whose commit the client never heard
// of: the SwappedTable it mints must say what the handoff left behind, and
// only the catalog knows.
func readLiveIdentities(ctx context.Context, pool *pgxpool.Pool, built BuiltShadow) ([]IdentityColumn, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("read live identities of %s.%s: %w", built.Schema(), built.SourceTable(), err)
	}
	defer func() {
		// Redundant safety closer: a read-only transaction that is
		// rolled back below either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	identities, err := readIdentityColumns(ctx, tx, built.ShadowOID())
	if err != nil {
		return nil, fmt.Errorf("live %s.%s: %w", built.Schema(), built.SourceTable(), err)
	}
	if err := tx.Rollback(ctx); err != nil {
		return nil, fmt.Errorf("read live identities of %s.%s: %w", built.Schema(), built.SourceTable(), err)
	}
	return identities, nil
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
