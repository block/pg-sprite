package schemachange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// runState is what dispatch hands the phases: the row the run writes
// from here on, the shadow it proved, and what the catalog said about the
// swap when the row was already past it.
type runState struct {
	// cp is the row as last saved, with the shadow proof it carries.
	cp checkpoint.Checkpoint
	// built is the shadow the run builds on; the zero value once the swap
	// is known to have committed, when no shadow bears the name any more.
	built BuiltShadow
	// proof is the plain view of the shadow, from built or from the row.
	proof Proof
	// swapped reports that the catalog shows the swap committed, so the
	// run continues at the old table drop; swappedTable is its proof.
	swapped      bool
	swappedTable SwappedTable
	// oldTableGone reports that the retained source is already gone, so
	// there is nothing to drop.
	oldTableGone bool
}

// dispatch reads the target's checkpoint row and starts fresh or resumes
// as it says, returning the state the phases continue from (ST-2).
func (r *Runner) dispatch(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, st statement.Statement) (runState, error) {
	cp, err := r.store.Inspect(ctx, target.Schema(), target.Table())
	if errors.Is(err, checkpoint.ErrNotFound) {
		return r.startFresh(ctx, pool, lock, target, st)
	}
	if err != nil {
		return runState{}, err
	}
	return r.resume(ctx, pool, lock, target, cp)
}

// startFresh builds the shadow and writes the row that records it, in
// PhaseCopying with nothing landed. A relation already under the shadow's
// name with no row to say what built it is refused: the row is the only
// record of which statement a shadow embodies, so without one the shadow
// is dropped and the run started over.
func (r *Runner) startFresh(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, st statement.Statement) (runState, error) {
	built, err := BuildShadow(ctx, pool, lock, target, st, r.opts.Shadow)
	if errors.Is(err, ErrShadowExists) {
		// INV: ST-2
		return runState{}, refuse(CauseShadowUnrecorded, []error{err}, "%s.%s has a relation under its shadow's name and no checkpoint row; the row is the only record of what built it", target.Schema(), target.Table())
	}
	if err != nil {
		return runState{}, err
	}
	state, err := newRunState(built)
	if err != nil {
		return runState{}, err
	}
	if err := r.save(ctx, lock, &state, checkpoint.PhaseCopying); err != nil {
		return runState{}, err
	}
	return state, nil
}

// newRunState is the state of a run that has just built its shadow.
func newRunState(built BuiltShadow) (runState, error) {
	proofJSON, err := json.Marshal(built)
	if err != nil {
		return runState{}, fmt.Errorf("encode shadow proof for %s.%s: %w", built.Schema(), built.SourceTable(), err)
	}
	return runState{
		cp: checkpoint.Checkpoint{
			Schema:            built.Schema(),
			Table:             built.SourceTable(),
			ShadowTable:       built.ShadowTable(),
			SourceFingerprint: built.SourceFingerprint(),
			TargetFingerprint: built.TargetFingerprint(),
			ShadowProof:       proofJSON,
		},
		built: built,
		proof: built.Proof(),
	}, nil
}

// resume continues the run cp records. The row's phase says which
// operation reads it; a row this runner cannot act on is refused before
// anything is written.
func (r *Runner) resume(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, cp checkpoint.Checkpoint) (runState, error) {
	if err := checkQuiescedRow(target, cp); err != nil {
		return runState{}, err
	}
	proof, err := rowProof(cp)
	if err != nil {
		return runState{}, err
	}
	if cp.Phase == checkpoint.PhaseCutover {
		// INV: LK-4 — a cutover row is answered by the catalog, never assumed.
		swapped, err := InspectSwapped(ctx, pool, lock, proof, r.opts.Shadow)
		switch {
		case err == nil:
			return runState{cp: cp, proof: proof, swapped: true, swappedTable: swapped}, nil
		case errors.Is(err, ErrOldTableNotFound):
			return runState{cp: cp, proof: proof, swapped: true, oldTableGone: true}, nil
		case errors.Is(err, ErrNotSwapped):
			// The shadow still bears its name; the run is back before its
			// cutover and continues from the shadow below.
		default:
			return runState{}, err
		}
	}
	built, err := InspectShadow(ctx, pool, lock, target, r.opts.Shadow)
	if errors.Is(err, ErrShadowNotFound) {
		return runState{}, fmt.Errorf("checkpoint row for %s.%s is in phase %s but %w; delete the row to start over", cp.Schema, cp.Table, cp.Phase, err)
	}
	if err != nil {
		return runState{}, err
	}
	if err := checkRowDescribes(cp, proof, built); err != nil {
		return runState{}, err
	}
	return runState{cp: cp, built: built, proof: proof}, nil
}

// checkQuiescedRow refuses a row the quiesced runner cannot act on: one in
// a terminal phase, whose run has ended; and one that belongs to a run
// that decodes WAL — it names a slot, or is in a phase only such a run
// enters — since continuing it without its slot would leave the slot
// behind as nobody's and the changes it carried unapplied.
func checkQuiescedRow(target preflight.CopySwapTarget, cp checkpoint.Checkpoint) error {
	if cp.Phase.Terminal() {
		return refuse(CauseResumeTerminal, nil, "checkpoint row for %s.%s is in phase %s", cp.Schema, cp.Table, cp.Phase)
	}
	// INV: ST-3
	if cp.SlotName != "" || cp.PublicationName != "" {
		return refuse(CauseResumeRowDecoding, nil, "checkpoint row for %s.%s names slot %s; a quiesced run has none", cp.Schema, cp.Table, cp.SlotName)
	}
	if cp.Phase == checkpoint.PhaseCatchingUp || cp.Phase == checkpoint.PhaseReconciling {
		return refuse(CauseResumeRowDecoding, nil, "checkpoint row for %s.%s is in phase %s, which only a run that decodes WAL enters", cp.Schema, cp.Table, cp.Phase)
	}
	if cp.Schema != target.Schema() || cp.Table != target.Table() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row is for %s.%s, target is %s.%s", cp.Schema, cp.Table, target.Schema(), target.Table())
	}
	return nil
}

// rowProof decodes the shadow proof the row carries. A row without one
// cannot be resumed: the proof is what the shadow is compared with, and
// after the swap it is the only record of which relations moved.
func rowProof(cp checkpoint.Checkpoint) (Proof, error) {
	if len(cp.ShadowProof) == 0 {
		return Proof{}, refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s carries no shadow proof", cp.Schema, cp.Table)
	}
	var proof Proof
	if err := json.Unmarshal(cp.ShadowProof, &proof); err != nil {
		return Proof{}, refuse(CauseResumeRowMismatch, []error{err}, "checkpoint row for %s.%s carries a shadow proof that does not decode", cp.Schema, cp.Table)
	}
	if proof.Schema != cp.Schema || proof.SourceTable != cp.Table {
		return Proof{}, refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s carries the shadow proof of %s.%s", cp.Schema, cp.Table, proof.Schema, proof.SourceTable)
	}
	return proof, nil
}

// checkRowDescribes refuses a row that does not describe the shadow the
// catalog holds: the names and fingerprints the row is keyed on must be
// the shadow's, and the proof the row carries must equal the proof the
// inspection re-derived, field for field. Both proofs are compared in
// their JSON encoding after a decode, since the row's came back as the
// server renders JSON and the inspection's has not been through it.
func checkRowDescribes(cp checkpoint.Checkpoint, rowProof Proof, built BuiltShadow) error {
	if cp.ShadowTable != built.ShadowTable() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s names shadow %s, the shadow under the derived name is %s", cp.Schema, cp.Table, cp.ShadowTable, built.ShadowTable())
	}
	if cp.SourceFingerprint != built.SourceFingerprint() || cp.TargetFingerprint != built.TargetFingerprint() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s carries fingerprints of another statement than the shadow embodies", cp.Schema, cp.Table)
	}
	same, err := sameProof(rowProof, built.Proof())
	if err != nil {
		return err
	}
	if !same {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s carries a shadow proof other than the one the shadow under its name re-derives", cp.Schema, cp.Table)
	}
	return nil
}

// sameProof compares two proofs by their canonical JSON encoding.
func sameProof(a, b Proof) (bool, error) {
	aJSON, err := json.Marshal(a)
	if err != nil {
		return false, fmt.Errorf("encode shadow proof: %w", err)
	}
	bJSON, err := json.Marshal(b)
	if err != nil {
		return false, fmt.Errorf("encode shadow proof: %w", err)
	}
	return bytes.Equal(aJSON, bJSON), nil
}
