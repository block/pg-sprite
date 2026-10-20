package schemachange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
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
	// reconciled reports that the run replaced a lost slot in reconcile
	// mode before its stream reopened; reconciliation is that recopy
	// pass's outcome.
	reconciled     bool
	reconciliation checksum.Outcome
}

// dispatch reads the target's checkpoint row and starts fresh or resumes
// as it says, returning the state the phases continue from (ST-2). A done
// row is replaced first when RunOptions.ReplaceDone asks for it, and any
// other row — or a shadow no row records — is discarded first when
// RunOptions.Restart asks for it; the run then starts fresh as if there
// had been none.
func (r *Runner) dispatch(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, st statement.Statement) (runState, error) {
	cp, err := r.store.Inspect(ctx, target.Schema(), target.Table())
	switch {
	case errors.Is(err, checkpoint.ErrNotFound) && r.opts.Restart:
		if err := r.discardUnrecorded(ctx, pool, lock, target); err != nil {
			return runState{}, err
		}
	case errors.Is(err, checkpoint.ErrNotFound):
	case err != nil:
		return runState{}, err
	case cp.Phase == checkpoint.PhaseDone && r.opts.ReplaceDone:
		if err := r.replaceDoneRow(ctx, pool, lock, target, cp); err != nil {
			return runState{}, err
		}
	case cp.Phase != checkpoint.PhaseDone && r.opts.Restart:
		if err := r.discardRow(ctx, pool, lock, target, cp); err != nil {
			return runState{}, err
		}
	default:
		if target.DecodesWAL() {
			return r.resumeLive(ctx, pool, lock, target, cp, st)
		}
		return r.resume(ctx, pool, lock, target, cp, st)
	}
	if target.DecodesWAL() {
		return r.startFreshLive(ctx, pool, lock, target, st)
	}
	return r.startFresh(ctx, pool, lock, target, st)
}

// replaceDoneRow deletes the done row cp, under this run's lock session,
// so the run can start fresh (RunOptions.ReplaceDone). The one thing a
// done row can still stand for is a KeepOld rollback window: the retained
// source under its derived _old name, which the next run's cutover gate
// would meet as cutover-name-taken only after a shadow had been built and
// filled. So the row is replaced only when nothing bears that name, and
// is otherwise refused as terminal with the remedy in the detail. The
// delete removes only the row this run was shown (ST-2).
func (r *Runner) replaceDoneRow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, cp checkpoint.Checkpoint) error {
	schema, table := target.Schema(), target.Table()
	old := OldName(schema, table)
	held, err := relationBearsName(ctx, pool, lock, schema, old, r.opts.Shadow)
	if err != nil {
		return err
	}
	if held {
		// INV: ST-4
		return refuse(CauseResumeTerminal, nil, "checkpoint row for %s.%s is in phase %s and the retained source still stands as %s.%s; end the rollback window — drop the old table with DropOldTable under the table's lock — before running the table again", schema, table, cp.Phase, schema, old)
	}
	if err := r.store.Delete(ctx, lock, schema, table, cp.Identity()); err != nil {
		return fmt.Errorf("replace done row for %s.%s: %w", schema, table, err)
	}
	return nil
}

// relationBearsName reports whether any relation in schema bears name, in
// one bounded read-only transaction that confirms the table lock (LK-1).
func relationBearsName(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, schema, name string, opts Options) (bool, error) {
	ctx, stop := lock.Bind(ctx)
	defer stop()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, fmt.Errorf("begin relation lookup for %s.%s: %w", schema, name, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBudgets(ctx, tx, opts); err != nil {
		return false, err
	}
	if err := setSearchPath(ctx, tx, schema); err != nil {
		return false, err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return false, lockLossCause(lock, err)
	}
	_, found, err := lookupRelation(ctx, tx, schema, name)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit relation lookup for %s.%s: %w", schema, name, err)
	}
	return found, nil
}

// startFresh builds the shadow, fences the source, and writes the row
// that records the shadow, in PhaseCopying with nothing landed. The fence
// goes on before the row so that a run stopped at its first save leaves
// the source fenced, as every later stop does. A relation already under
// the shadow's name with no row to say what built it is refused: the row
// is the only record of which statement a shadow embodies, so without one
// the shadow is dropped and the run started over. For the same reason a
// first save whose row does not land drops the shadow and the fence this
// run just put there: leaving them would make the next Run meet that
// refusal for a shadow nothing records. A row that landed stays, with the
// shadow and the fence it records, whatever AfterCheckpoint then returns.
func (r *Runner) startFresh(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, st statement.Statement) (runState, error) {
	built, err := BuildShadow(ctx, pool, lock, target, st, r.opts.Shadow)
	if errors.Is(err, ErrShadowExists) {
		// INV: ST-2
		return runState{}, refuse(CauseShadowUnrecorded, []error{err}, "%s.%s has a relation under its shadow's name and no checkpoint row; the row is the only record of what built it", target.Schema(), target.Table())
	}
	if err != nil {
		return runState{}, err
	}
	state, err := newRunState(built, st)
	if err != nil {
		return runState{}, err
	}
	// INV: CO-1 — the source takes no write from here to the swap.
	if err := installWriteFence(ctx, pool, lock, target, r.opts.Shadow); err != nil {
		return runState{}, errors.Join(err, r.unbuild(ctx, pool, lock, target))
	}
	state.cp.Phase = checkpoint.PhaseCopying
	if err := r.store.Save(ctx, lock, state.cp); err != nil {
		return runState{}, errors.Join(err, r.unbuild(ctx, pool, lock, target))
	}
	if err := r.afterCheckpoint(ctx, state.cp); err != nil {
		return runState{}, err
	}
	return state, nil
}

// unbuild takes back what startFresh did before its row landed: the fence
// comes off the source and the shadow is dropped. It outlives the
// caller's context, since a cancelled run is the usual way here, but is
// bounded so it returns against a server that has stopped answering.
func (r *Runner) unbuild(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget) error {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
	defer cancel()
	return errors.Join(
		DropWriteFence(undoCtx, pool, lock, target, r.opts.Shadow),
		DropShadow(undoCtx, pool, lock, target, r.opts.Shadow),
	)
}

// newRunState is the state of a run that has just built its shadow from st.
func newRunState(built BuiltShadow, st statement.Statement) (runState, error) {
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
			Statement:         st.SQL(),
		},
		built: built,
		proof: built.Proof(),
	}, nil
}

// resume continues the quiesced run cp records. A row this runner cannot
// act on, or one that records another statement than st, is refused
// before anything is written.
func (r *Runner) resume(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, cp checkpoint.Checkpoint, st statement.Statement) (runState, error) {
	if err := checkQuiescedRow(target, cp); err != nil {
		return runState{}, err
	}
	state, err := r.resumeOnRow(ctx, pool, lock, target, cp, st)
	if err != nil {
		return runState{}, err
	}
	if state.swapped {
		return state, nil
	}
	// INV: CO-1 — the resumed run installs over the fence the stopped run
	// left, so the source takes no write from here to the swap and the
	// copy and the verification pass are the whole proof.
	if err := installWriteFence(ctx, pool, lock, target, r.opts.Shadow); err != nil {
		return runState{}, err
	}
	return state, nil
}

// resumeOnRow reads the catalog for the run cp records, once the row is
// known to be this runner's kind: the row's phase says which operation
// reads it, and a row that records another statement than st, or that
// does not describe the shadow under its name, is refused before anything
// is written.
func (r *Runner) resumeOnRow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, cp checkpoint.Checkpoint, st statement.Statement) (runState, error) {
	if err := checkRowStatement(target, cp, st); err != nil {
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
	built, err := InspectShadow(ctx, pool, lock, target, st, r.opts.Shadow)
	if errors.Is(err, ErrShadowNotFound) {
		// INV: ST-2 — the refusal writes nothing: a live row's slot stays
		// with the row for the operator to drop, as it does for every
		// other resume refusal.
		return runState{}, refuse(CauseResumeShadowGone, []error{err}, "checkpoint row for %s.%s is in phase %s but no relation bears its shadow name %s; %s", cp.Schema, cp.Table, cp.Phase, cp.ShadowTable, shadowGoneRemedy(cp))
	}
	if err != nil {
		return runState{}, err
	}
	if err := checkRowDescribes(cp, proof, built); err != nil {
		return runState{}, err
	}
	return runState{cp: cp, built: built, proof: proof}, nil
}

// shadowGoneRemedy says how to start over from a row whose shadow is gone.
// A live row also names a slot, which retains WAL while it stands and
// which a fresh run would meet when it creates its own.
func shadowGoneRemedy(cp checkpoint.Checkpoint) string {
	if cp.SlotName == "" {
		return "delete the row to start over"
	}
	return fmt.Sprintf("drop its slot %s and delete the row to start over", cp.SlotName)
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

// checkRowStatement refuses a resume under a statement other than the one
// the row records: a resume completes the change a stopped run started,
// and a caller that means another change on the table deletes the row
// first. The two are compared as parsed — the same operations on the
// same table — so a statement that only differs in its spelling, or in
// whether it qualifies the table with the target's schema, resumes. A row
// without a statement cannot prove what it records and is refused the
// same way.
func checkRowStatement(target preflight.CopySwapTarget, cp checkpoint.Checkpoint, st statement.Statement) error {
	if cp.Statement == "" {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s records no statement; delete the row to start over", cp.Schema, cp.Table)
	}
	if !namesTarget(st, target.Schema(), target.Table()) {
		return refuse(CauseStatementTarget, nil, "statement names %s.%s, the proven table is %s.%s", st.Schema(), st.Table(), target.Schema(), target.Table())
	}
	if err := statement.SameOpsExceptTarget(cp.Statement, st.SQL()); err != nil {
		// INV: ST-2
		return refuse(CauseResumeRowMismatch, []error{err}, "checkpoint row for %s.%s records a run of another statement; delete the row to run this one", cp.Schema, cp.Table)
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
