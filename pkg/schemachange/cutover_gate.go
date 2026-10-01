package schemachange

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// CutoverReady is the proof that the shadow may be swapped in: the data
// proof (CO-1) and the ST-5 fidelity checklist both held against the live
// catalog, and the facts the swap itself needs — which shadow dependent
// takes which source name (D8) and which sequences change owner (D5) — were
// read in the same transaction. Its constructor is private: only GateCutover
// mints one, and the cutover accepts nothing else.
type CutoverReady struct {
	built      BuiltShadow
	verified   checksum.VerifiedShadow
	indexes    DependentPairing
	statistics DependentPairing
	sequences  []OwnedSequence
}

// Built is the shadow proof the gate held against the catalog.
func (r CutoverReady) Built() BuiltShadow { return r.built }

// Verified is the data-equality proof the gate accepted.
func (r CutoverReady) Verified() checksum.VerifiedShadow { return r.verified }

// Indexes pairs the source's indexes with the shadow's by definition.
func (r CutoverReady) Indexes() DependentPairing { return r.indexes }

// Statistics pairs the source's extended-statistics objects with the
// shadow's by definition.
func (r CutoverReady) Statistics() DependentPairing { return r.statistics }

// OwnedSequences are the sequences the source's columns own, which cutover
// re-owns to the live table.
func (r CutoverReady) OwnedSequences() []OwnedSequence {
	return append([]OwnedSequence(nil), r.sequences...)
}

// GateCutover runs the ST-5 checklist against the live catalog, in one
// bounded read-only transaction under SET LOCAL ROLE owner, and mints the
// CutoverReady proof when every item holds. It refuses, fail-closed, when:
// the verified-shadow proof is missing, names other relations, or stops
// short of the whole key space; the source or the shadow is no longer the
// relation the build proved; a shadow index is invalid; either table's
// introspected model or metadata snapshot drifted from what the build
// recorded; a shadow identity column no longer draws from the source's
// sequence; or a name the swap must assign is already taken. It executes
// nothing and holds no lock beyond the per-table session; the cutover
// re-runs the same checklist inside its own transaction, under ACCESS
// EXCLUSIVE, before the first rename.
//
// The caller holds the per-table lock (LK-1): the gate runs under the lock
// session's Bind context, and its transaction confirms from its own
// connection that the lock session's backend holds the table.
func GateCutover(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, built BuiltShadow, verified checksum.VerifiedShadow, opts Options) (CutoverReady, error) {
	if err := opts.validate(); err != nil {
		return CutoverReady{}, err
	}
	if err := checkCutoverProofs(built, verified); err != nil {
		return CutoverReady{}, err
	}
	if err := requireTableLock(lock, built.Schema(), built.SourceTable()); err != nil {
		return CutoverReady{}, err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	ready, err := gateCutover(ctx, pool, lock, built, verified, opts)
	if err != nil {
		return CutoverReady{}, lockLossCause(lock, err)
	}
	return ready, nil
}

func gateCutover(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, built BuiltShadow, verified checksum.VerifiedShadow, opts Options) (CutoverReady, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return CutoverReady{}, fmt.Errorf("begin cutover gate: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setGateSession(ctx, tx, built, opts); err != nil {
		return CutoverReady{}, err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return CutoverReady{}, err
	}
	ready, err := gateCutoverTx(ctx, tx, built, verified)
	if err != nil {
		return CutoverReady{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CutoverReady{}, fmt.Errorf("commit cutover gate: %w", err)
	}
	return ready, nil
}

// gateCutoverTx is the checklist proper, run inside a transaction the caller
// has bounded and whose lock it has confirmed: the standalone gate runs it
// in its own read-only transaction, and the cutover runs it again inside
// the swap transaction so the proof it acts on is as fresh as the lock.
func gateCutoverTx(ctx context.Context, tx pgx.Tx, built BuiltShadow, verified checksum.VerifiedShadow) (CutoverReady, error) {
	schema, source, shadow := built.Schema(), built.SourceTable(), built.ShadowTable()
	if err := confirmRelations(ctx, tx, built); err != nil {
		return CutoverReady{}, err
	}
	shadowIndexes, err := readIndexes(ctx, tx, built.ShadowOID())
	if err != nil {
		return CutoverReady{}, fmt.Errorf("shadow %s.%s: %w", schema, shadow, err)
	}
	if invalid := invalidIndexes(shadowIndexes); len(invalid) > 0 {
		// INV: ST-5
		return CutoverReady{}, refuse(CauseIndexInvalid, nil, "shadow %s.%s has invalid indexes: %s", schema, shadow, joinNames(invalid))
	}
	sourceModel, err := schemadiff.IntrospectTx(ctx, tx, schema, source)
	if err != nil {
		return CutoverReady{}, fmt.Errorf("introspect source %s.%s: %w", schema, source, err)
	}
	targetModel, err := schemadiff.IntrospectTx(ctx, tx, schema, shadow)
	if err != nil {
		return CutoverReady{}, fmt.Errorf("introspect shadow %s.%s: %w", schema, shadow, err)
	}
	if err := confirmFingerprints(built, sourceModel, targetModel); err != nil {
		return CutoverReady{}, err
	}
	if err := confirmFidelity(ctx, tx, built); err != nil {
		return CutoverReady{}, err
	}
	if err := confirmIdentities(ctx, tx, built, targetModel); err != nil {
		return CutoverReady{}, err
	}
	sourceIndexes, err := readIndexes(ctx, tx, built.SourceOID())
	if err != nil {
		return CutoverReady{}, fmt.Errorf("source %s.%s: %w", schema, source, err)
	}
	indexes, err := pairIndexes(sourceIndexes, shadowIndexes)
	if err != nil {
		return CutoverReady{}, err
	}
	statistics, err := pairStatistics(ctx, tx, built)
	if err != nil {
		return CutoverReady{}, err
	}
	if err := confirmNamesFree(ctx, tx, built, indexes, statistics, shadowIndexes); err != nil {
		return CutoverReady{}, err
	}
	sequences, err := readOwnedSequences(ctx, tx, built.SourceOID())
	if err != nil {
		return CutoverReady{}, fmt.Errorf("source %s.%s: %w", schema, source, err)
	}
	return CutoverReady{
		built:      built,
		verified:   verified,
		indexes:    indexes,
		statistics: statistics,
		sequences:  sequences,
	}, nil
}

// checkCutoverProofs refuses proofs that cannot describe one verified copy:
// an empty built shadow, an empty verified shadow, two proofs naming
// different relations, or a verification that stopped short of the whole
// key space and so proves only a prefix.
func checkCutoverProofs(built BuiltShadow, verified checksum.VerifiedShadow) error {
	// INV: CO-1
	if built.ShadowTable() == "" {
		return refuse(CauseCutoverUnverified, nil, "built shadow proof is empty")
	}
	if verified.Table() == "" {
		return refuse(CauseCutoverUnverified, nil, "verified shadow proof is empty")
	}
	if verified.Schema() != built.Schema() || verified.Table() != built.SourceTable() || verified.Shadow() != built.ShadowTable() {
		return refuse(CauseCutoverUnverified, nil, "verified shadow proof is for %s.%s into %s, built shadow is %s.%s into %s",
			verified.Schema(), verified.Table(), verified.Shadow(), built.Schema(), built.SourceTable(), built.ShadowTable())
	}
	if !verified.Watermark().Complete() {
		return refuse(CauseCutoverUnverified, nil, "verified shadow proof covers the key space only through %d", verified.Watermark().Value())
	}
	return nil
}

// setGateSession bounds the transaction and puts it in the owner's shoes,
// as the build session does; the owner comes from the proof's snapshot.
func setGateSession(ctx context.Context, tx pgx.Tx, built BuiltShadow, opts Options) error {
	return setSession(ctx, tx, built.Schema(), built.Fidelity().Owner, opts)
}

// confirmRelations refuses a source or shadow whose OID moved since the
// build: the name now belongs to a relation the proof does not describe.
func confirmRelations(ctx context.Context, tx pgx.Tx, built BuiltShadow) error {
	schema := built.Schema()
	sourceOID, err := resolveRelation(ctx, tx, schema, built.SourceTable())
	if err != nil {
		return err
	}
	// INV: ST-6
	if sourceOID != built.SourceOID() {
		return refuse(CauseRelationReplaced, nil, "source %s.%s is relation %d, the build proved %d", schema, built.SourceTable(), sourceOID, built.SourceOID())
	}
	shadowOID, err := resolveRelation(ctx, tx, schema, built.ShadowTable())
	if err != nil {
		return err
	}
	if shadowOID != built.ShadowOID() {
		return refuse(CauseRelationReplaced, nil, "shadow %s.%s is relation %d, the build proved %d", schema, built.ShadowTable(), shadowOID, built.ShadowOID())
	}
	return nil
}

// confirmFingerprints refuses either table whose introspected model no
// longer digests to what the build recorded.
func confirmFingerprints(built BuiltShadow, sourceModel, targetModel schemadiff.Model) error {
	sourceFingerprint, err := fingerprint(sourceModel)
	if err != nil {
		return err
	}
	// INV: ST-5
	if sourceFingerprint != built.SourceFingerprint() {
		return refuse(CauseSchemaDrift, nil, "source %s.%s changed shape since the shadow was built", built.Schema(), built.SourceTable())
	}
	targetFingerprint, err := fingerprint(targetModel)
	if err != nil {
		return err
	}
	if targetFingerprint != built.TargetFingerprint() {
		return refuse(CauseSchemaDrift, nil, "shadow %s.%s changed shape since it was built", built.Schema(), built.ShadowTable())
	}
	return nil
}

// confirmFidelity re-reads both metadata snapshots and refuses a table
// whose snapshot drifted from the build's, naming the drifted facts.
func confirmFidelity(ctx context.Context, tx pgx.Tx, built BuiltShadow) error {
	source, err := readFidelity(ctx, tx, built.SourceOID())
	if err != nil {
		return fmt.Errorf("source %s.%s: %w", built.Schema(), built.SourceTable(), err)
	}
	drifted, err := fidelityDrift(source, built.Fidelity())
	if err != nil {
		return err
	}
	// INV: ST-5
	if len(drifted) > 0 {
		return refuse(CauseFidelityDrift, nil, "source %s.%s metadata changed since the shadow was built: %s", built.Schema(), built.SourceTable(), joinNames(drifted))
	}
	shadow, err := readFidelity(ctx, tx, built.ShadowOID())
	if err != nil {
		return fmt.Errorf("shadow %s.%s: %w", built.Schema(), built.ShadowTable(), err)
	}
	drifted, err = fidelityDrift(shadow, built.ShadowFidelity())
	if err != nil {
		return err
	}
	if len(drifted) > 0 {
		return refuse(CauseFidelityDrift, nil, "shadow %s.%s metadata changed since it was built: %s", built.Schema(), built.ShadowTable(), joinNames(drifted))
	}
	return nil
}

// confirmIdentities refuses a shadow identity handoff that no longer holds,
// and a source identity sequence whose options changed since the build —
// cutover replays those options into the live table's identity, so they
// must be the ones the proof recorded.
func confirmIdentities(ctx context.Context, tx pgx.Tx, built BuiltShadow, targetModel schemadiff.Model) error {
	if err := verifyIdentityDefaults(ctx, tx, built.ShadowOID(), built.IdentityColumns()); err != nil {
		return err
	}
	live, err := readIdentityColumns(ctx, tx, built.SourceOID())
	if err != nil {
		return err
	}
	// INV: ST-5
	if !slices.Equal(handoffIdentities(live, targetModel), built.IdentityColumns()) {
		return refuse(CauseFidelityDrift, nil, "source %s.%s identity columns or their sequence options changed since the shadow was built", built.Schema(), built.SourceTable())
	}
	return nil
}

// pairIndexes pairs the two tables' indexes by definition.
func pairIndexes(source, shadow []indexEntry) (DependentPairing, error) {
	sourceDependents, err := indexDependents(source)
	if err != nil {
		return DependentPairing{}, err
	}
	shadowDependents, err := indexDependents(shadow)
	if err != nil {
		return DependentPairing{}, err
	}
	return pairByDefinition(DependentIndex, sourceDependents, shadowDependents), nil
}

// pairStatistics pairs the two tables' extended-statistics objects by
// definition.
func pairStatistics(ctx context.Context, tx pgx.Tx, built BuiltShadow) (DependentPairing, error) {
	source, err := readStatistics(ctx, tx, built.SourceOID())
	if err != nil {
		return DependentPairing{}, fmt.Errorf("source %s.%s: %w", built.Schema(), built.SourceTable(), err)
	}
	shadow, err := readStatistics(ctx, tx, built.ShadowOID())
	if err != nil {
		return DependentPairing{}, fmt.Errorf("shadow %s.%s: %w", built.Schema(), built.ShadowTable(), err)
	}
	return pairByDefinition(DependentStatistics, source, shadow), nil
}

// confirmNamesFree refuses a swap whose renames would collide: a derived
// _old name for the source, one of its dependents, or one of its identity
// sequences that some relation or statistics object already wears, or a
// constraint-backed source index name that another constraint on the
// shadow already holds.
func confirmNamesFree(ctx context.Context, tx pgx.Tx, built BuiltShadow, indexes, statistics DependentPairing, shadowIndexes []indexEntry) error {
	schema, source := built.Schema(), built.SourceTable()
	dependents := append(indexes.sourceNames(), statistics.sourceNames()...)
	for _, id := range built.IdentityColumns() {
		dependents = append(dependents, id.SequenceName)
	}
	taken, err := takenNames(ctx, tx, schema, oldNames(schema, source, dependents))
	if err != nil {
		return err
	}
	// INV: ST-5
	if len(taken) > 0 {
		return refuse(CauseNameTaken, nil, "names cutover assigns to the retained %s.%s are already taken: %s", schema, source, joinNames(taken))
	}
	taken, err = constraintNamesTaken(ctx, tx, built.ShadowOID(), indexes, shadowIndexes)
	if err != nil {
		return err
	}
	if len(taken) > 0 {
		return refuse(CauseNameTaken, nil, "shadow %s.%s already has constraints named as source indexes it must take over: %s", schema, built.ShadowTable(), joinNames(taken))
	}
	return nil
}
