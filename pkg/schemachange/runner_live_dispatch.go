package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// startFreshLive builds the shadow, creates the slot, and writes the row
// that records both, in PhaseCopying with nothing landed and the slot's
// consistent point as the last applied position. The slot is made and the
// row saved under the engine's lock on the slot's name, so the reaper
// never meets the slot before the row that owns it (ST-3). A first save
// whose row does not land drops the slot and the shadow this run just
// made, as the quiesced start drops its shadow and fence: nothing records
// them, so the next Run would refuse the shadow as shadow-unrecorded and
// the reaper would drop the slot. Only what this run made is taken back:
// a slot of the derived name that was there before the run — an orphan
// of a run that ended without its row, or another tool's — is reported
// as the *decode.SlotExistsError it is and left as found, for the reaper
// or the operator. A row that landed stays, with both, whatever
// AfterCheckpoint then returns.
func (r *Runner) startFreshLive(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, st statement.Statement) (runState, error) {
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
	name := target.DecodingName()
	state.cp.Phase = checkpoint.PhaseCopying
	state.cp.SlotName = name
	state.cp.PublicationName = name
	// CreateSlot leaves no slot behind an error, so the slot is this
	// run's to drop only once it has returned one.
	slotMade := false
	err = r.store.WithSlotLock(ctx, r.cfg, name, func(ctx context.Context) error {
		slot, err := decode.CreateSlot(ctx, r.cfg, pool, target)
		if err != nil {
			return err
		}
		slotMade = true
		// INV: ST-4 — the row carries the position decoding resumes from
		// before any change is captured.
		state.cp.LastAppliedLSN = slot.ConsistentPoint()
		return errors.Join(r.store.Save(ctx, lock, state.cp), slot.Close(context.WithoutCancel(ctx)))
	})
	if err != nil {
		return runState{}, errors.Join(err, r.unbuildLive(ctx, pool, lock, target, slotMade))
	}
	if err := r.afterCheckpoint(ctx, state.cp); err != nil {
		return runState{}, err
	}
	return state, nil
}

// unbuildLive takes back what startFreshLive did before its row landed:
// the slot, when dropSlot says the attempt made one, and the shadow. A
// slot the attempt did not make is not this run's to drop (ST-3). It
// outlives the caller's context, since a cancelled run is the usual way
// here, but is bounded so it returns against a server that has stopped
// answering.
func (r *Runner) unbuildLive(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, dropSlot bool) error {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
	defer cancel()
	var slotErr error
	if dropSlot {
		slotErr = decode.DropSlot(undoCtx, r.cfg, pool, target.DecodingName())
	}
	return errors.Join(slotErr, DropShadow(undoCtx, pool, lock, target, r.opts.Shadow))
}

// resumeLive continues the live run cp records. The row's slot is not
// read here: a row before its cutover reads it, and reconciles a lost
// one, once the run is about to open its stream (liveReady), so a row
// whose swap already committed never touches the slot it is about to
// drop.
func (r *Runner) resumeLive(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, cp checkpoint.Checkpoint, st statement.Statement) (runState, error) {
	if err := checkLiveRow(target, cp); err != nil {
		return runState{}, err
	}
	return r.resumeOnRow(ctx, pool, lock, target, cp, st)
}

// checkLiveRow refuses a row the live runner cannot act on: one in a
// terminal phase, whose run has ended; one that belongs to a quiesced
// run — it names no slot — since the source behind it was fenced, not
// captured, and no stream could replay what it missed; and one naming a
// slot other than the target's derived name, which is not this route's.
func checkLiveRow(target preflight.CopySwapTarget, cp checkpoint.Checkpoint) error {
	if cp.Phase.Terminal() {
		return refuse(CauseResumeTerminal, nil, "checkpoint row for %s.%s is in phase %s", cp.Schema, cp.Table, cp.Phase)
	}
	// INV: ST-3
	if cp.SlotName == "" || cp.PublicationName == "" {
		return refuse(CauseResumeRowDecoding, nil, "checkpoint row for %s.%s names no slot; a run that decodes WAL resumes only a row that names its own", cp.Schema, cp.Table)
	}
	if cp.Schema != target.Schema() || cp.Table != target.Table() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row is for %s.%s, target is %s.%s", cp.Schema, cp.Table, target.Schema(), target.Table())
	}
	if cp.SlotName != target.DecodingName() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s names slot %s, the target's derived name is %s", cp.Schema, cp.Table, cp.SlotName, target.DecodingName())
	}
	return nil
}

// liveReady makes sure the row's slot can be decoded from before the run
// opens its stream (InspectResume): a slot that is gone or that the
// server reports lost is replaced in reconcile mode (ST-4), which leaves
// the row in the phase its watermark implies with the new slot's
// consistent point as the position to resume from. The reconcile's own
// saves go through the store alone; the row it leaves is handed to
// AfterCheckpoint once, as the run's state.
func (r *Runner) liveReady(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, state *runState) error {
	resume, err := InspectResume(ctx, pool, target, state.cp)
	if err != nil {
		return err
	}
	if resume.Mode == ResumeClean {
		return nil
	}
	reconciler, err := NewReconciler(r.cfg, lock, r.store, target, state.built, r.opts.Checksum)
	if err != nil {
		return err
	}
	reconciled, err := reconciler.Reconcile(ctx, pool, state.cp)
	if err != nil {
		return err
	}
	// The stream opens on its own replication connection at the row's
	// position; the one the slot was created on is not kept.
	if err := reconciled.Slot.Close(ctx); err != nil {
		return fmt.Errorf("reconcile %s.%s: %w", state.cp.Schema, state.cp.Table, err)
	}
	state.cp = reconciled.Checkpoint
	state.reconciled = true
	state.reconciliation = reconciled.Outcome
	return r.afterCheckpoint(ctx, state.cp)
}
