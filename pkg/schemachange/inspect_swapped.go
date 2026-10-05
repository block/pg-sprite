package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// ErrNotSwapped reports that the source still bears its name: no swap
// committed, so there is no swapped table to inspect and the caller runs
// Cutover.
var ErrNotSwapped = errors.New("source table is still live, not swapped")

// ErrOldTableNotFound reports that the shadow bears the source's name but
// nothing bears the derived _old name: the retained source is already
// gone, so the swap is complete and there is nothing left for DropOldTable
// to do.
var ErrOldTableNotFound = errors.New("old table not found")

// InspectSwapped re-derives the SwappedTable proof for a swap an earlier
// run committed, from the catalog alone, in one bounded read transaction
// under SET LOCAL ROLE owner. It is the resume path for a run that stopped
// between Cutover and DropOldTable, and the one home of LK-4's committed
// branch. The expectation is the BuiltShadow proof the caller checkpointed
// before the cutover: only the catalog can say what the swap did, but only
// that proof can say which two relations the swap was about. The live
// name must be borne by the proof's shadow OID and the derived _old name
// by the proof's source OID (ST-6); the dependent renames are re-derived
// from the names the swap gave, each live dependent paired with the
// old-table dependent under its derived name; and every identity column
// the proof handed off must be an identity column on the live table
// drawing from the sequence the handoff recreated under the source
// sequence's name.
//
// The catalog states the proof does not describe each get their own
// answer: the source still live is ErrNotSwapped, the _old name borne by
// nothing is ErrOldTableNotFound, the _old name borne by another relation
// is refused as cutover-relation-replaced, and the live name borne by
// neither relation is refused as cutover-outcome-ambiguous (LK-4).
func InspectSwapped(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, expected Proof, opts Options) (SwappedTable, error) {
	if err := opts.validate(); err != nil {
		return SwappedTable{}, err
	}
	if expected.ShadowTable == "" {
		return SwappedTable{}, refuse(CauseProofEmpty, nil, "built shadow proof is empty")
	}
	if err := requireTableLock(lock, expected.Schema, expected.SourceTable); err != nil {
		return SwappedTable{}, err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	swapped, err := inspectSwapped(ctx, pool, lock, expected, opts)
	if err != nil {
		return SwappedTable{}, lockLossCause(lock, err)
	}
	return swapped, nil
}

func inspectSwapped(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, expected Proof, opts Options) (SwappedTable, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return SwappedTable{}, fmt.Errorf("begin swapped table inspection: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setSession(ctx, tx, expected.Schema, expected.Fidelity.Owner, opts); err != nil {
		return SwappedTable{}, err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return SwappedTable{}, err
	}
	swapped, err := readSwappedTable(ctx, tx, expected)
	if err != nil {
		return SwappedTable{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SwappedTable{}, fmt.Errorf("commit swapped table inspection: %w", err)
	}
	return swapped, nil
}

// readSwappedTable reads the swap's outcome inside the caller's
// transaction and mints the proof when the catalog shows the committed
// swap the expectation describes. Attempts is zero: the inspection cannot
// know how many lock acquisitions the swap needed.
func readSwappedTable(ctx context.Context, tx pgx.Tx, expected Proof) (SwappedTable, error) {
	schema, table := expected.Schema, expected.SourceTable
	liveOID, err := confirmLive(ctx, tx, expected)
	if err != nil {
		return SwappedTable{}, err
	}
	oldOID, err := confirmOld(ctx, tx, expected)
	if err != nil {
		return SwappedTable{}, err
	}
	liveIndexes, err := readIndexes(ctx, tx, liveOID)
	if err != nil {
		return SwappedTable{}, fmt.Errorf("live %s.%s: %w", schema, table, err)
	}
	oldIndexes, err := readIndexes(ctx, tx, oldOID)
	if err != nil {
		return SwappedTable{}, fmt.Errorf("old %s.%s: %w", schema, OldName(schema, table), err)
	}
	liveStatistics, err := readStatistics(ctx, tx, liveOID)
	if err != nil {
		return SwappedTable{}, fmt.Errorf("live %s.%s: %w", schema, table, err)
	}
	oldStatistics, err := readStatistics(ctx, tx, oldOID)
	if err != nil {
		return SwappedTable{}, fmt.Errorf("old %s.%s: %w", schema, OldName(schema, table), err)
	}
	sequences, err := readOwnedSequences(ctx, tx, liveOID)
	if err != nil {
		return SwappedTable{}, fmt.Errorf("live %s.%s: %w", schema, table, err)
	}
	identities, err := readIdentityColumns(ctx, tx, liveOID)
	if err != nil {
		return SwappedTable{}, fmt.Errorf("live %s.%s: %w", schema, table, err)
	}
	if err := confirmHandoff(expected, identities); err != nil {
		return SwappedTable{}, err
	}
	return SwappedTable{
		schema:     schema,
		table:      table,
		liveOID:    liveOID,
		oldOID:     oldOID,
		owner:      expected.Fidelity.Owner,
		indexes:    pairByDerivedName(DependentIndex, schema, table, indexNames(oldIndexes), indexNames(liveIndexes)),
		statistics: pairByDerivedName(DependentStatistics, schema, table, dependentNames(oldStatistics), dependentNames(liveStatistics)),
		sequences:  sequences,
		identities: identities,
	}, nil
}

// confirmLive reads which relation bears the source's name and returns
// its OID when it is the shadow the build proved. The source's own OID
// means no swap committed; any other state — no relation, or one the build
// never proved — leaves the swap's outcome unreadable.
func confirmLive(ctx context.Context, tx pgx.Tx, expected Proof) (uint32, error) {
	schema, table := expected.Schema, expected.SourceTable
	liveOID, found, err := lookupRelation(ctx, tx, schema, table)
	if err != nil {
		return 0, err
	}
	if !found {
		// INV: LK-4
		return 0, refuse(CauseOutcomeAmbiguous, nil, "nothing bears the name %s.%s", schema, table)
	}
	if liveOID == expected.SourceOID {
		return 0, fmt.Errorf("%w: %s.%s is still relation %d", ErrNotSwapped, schema, table, liveOID)
	}
	if liveOID != expected.ShadowOID {
		// INV: LK-4
		return 0, refuse(CauseOutcomeAmbiguous, nil, "%s.%s is relation %d, which is neither the source %d nor the shadow %d", schema, table, liveOID, expected.SourceOID, expected.ShadowOID)
	}
	return liveOID, nil
}

// confirmOld reads which relation bears the derived _old name and returns
// its OID when it is the source the build proved. No relation means the
// retained source is already dropped; another relation is refused, since
// a drop by name would remove something the swap never retained.
func confirmOld(ctx context.Context, tx pgx.Tx, expected Proof) (uint32, error) {
	schema, old := expected.Schema, OldName(expected.Schema, expected.SourceTable)
	oldOID, found, err := lookupRelation(ctx, tx, schema, old)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("%w: %s.%s", ErrOldTableNotFound, schema, old)
	}
	// INV: ST-6
	if oldOID != expected.SourceOID {
		return 0, refuse(CauseRelationReplaced, nil, "old table %s.%s is relation %d, the swap retained %d", schema, old, oldOID, expected.SourceOID)
	}
	return oldOID, nil
}

// confirmHandoff refuses a live table on which an identity column the
// swap was to recreate is missing, generated differently, or drawing from
// a sequence other than the one recreated under the source sequence's
// name. The sequence options are not compared: a widened column carries
// rebased bounds by design.
func confirmHandoff(expected Proof, live []IdentityColumn) error {
	schema, table := expected.Schema, expected.SourceTable
	onLive := make(map[string]IdentityColumn, len(live))
	for _, id := range live {
		onLive[id.Column] = id
	}
	for _, want := range expected.IdentityColumns {
		got, found := onLive[want.Column]
		// INV: ST-6
		if !found {
			return refuse(CauseSwapMismatch, nil, "live %s.%s has no identity column %s the swap was to recreate", schema, table, want.Column)
		}
		if got.Always != want.Always {
			return refuse(CauseSwapMismatch, nil, "identity column %s on live %s.%s is not generated as the source's was", want.Column, schema, table)
		}
		if got.SequenceSchema != want.SequenceSchema || got.SequenceName != want.SequenceName {
			return refuse(CauseSwapMismatch, nil, "identity column %s on live %s.%s draws from %s.%s, the swap was to recreate it under %s.%s",
				want.Column, schema, table, got.SequenceSchema, got.SequenceName, want.SequenceSchema, want.SequenceName)
		}
	}
	return nil
}

// pairByDerivedName re-derives a pairing from the names the swap gave
// (D8): a live dependent bearing name N took it from the source dependent
// the swap renamed to the derived name of N, so the two pair when the old
// table carries that derived name. A live dependent with no such partner
// kept its shadow-derived name; an old-table dependent with no such
// partner is listed under the derived name it bears, since the hash the
// name was derived from has no inverse. The live order decides the pair
// order.
func pairByDerivedName(kind DependentKind, schema, table string, old, live []string) DependentPairing {
	unpairedOld := make(map[string]bool, len(old))
	for _, name := range old {
		unpairedOld[name] = true
	}
	pairing := DependentPairing{
		Pairs:          []DependentPair{},
		UnpairedSource: []string{},
		UnpairedShadow: []string{},
	}
	for _, name := range live {
		derived := OldDependentName(schema, table, name)
		if !unpairedOld[derived] {
			pairing.UnpairedShadow = append(pairing.UnpairedShadow, name)
			continue
		}
		pairing.Pairs = append(pairing.Pairs, DependentPair{Kind: kind, SourceName: name})
		delete(unpairedOld, derived)
	}
	for _, name := range old {
		if unpairedOld[name] {
			pairing.UnpairedSource = append(pairing.UnpairedSource, name)
		}
	}
	return pairing
}

// lookupRelation returns the OID of schema.name and whether any relation
// bears it; unlike resolveRelation, a missing relation is an answer here,
// not an error.
func lookupRelation(ctx context.Context, tx pgx.Tx, schema, name string) (uint32, bool, error) {
	var oid uint32
	err := tx.QueryRow(ctx, `
		SELECT c.oid
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, name).Scan(&oid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("look up relation %s.%s: %w", schema, name, err)
	}
	return oid, true, nil
}
