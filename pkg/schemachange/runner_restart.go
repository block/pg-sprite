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
)

// discardRow discards what the stopped run cp records, under this run's
// lock session, so the run can start fresh (RunOptions.Restart). The row
// is read first: a cutover row is answered by the catalog, and one whose
// swap committed is refused, since the shadow already bears the source's
// name and there is nothing to discard. Otherwise the hold the stopped
// run keeps is released in the order the next run would need were this
// one to stop partway — the shadow first, then the fence or the slot,
// then the row — so a row left standing still records a hold that is
// gone or going, and a later restart meets each step as idempotent. The
// row deleted is only the row this run was shown (ST-2). A row naming a
// slot other than the target's derived name is refused before anything
// is discarded, as a resume refuses it: it is not this target's run, and
// its slot is not the restart's to drop (ST-3).
func (r *Runner) discardRow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, cp checkpoint.Checkpoint) error {
	// INV: ST-3 — a restart drops only the target's own slot.
	if cp.SlotName != "" && cp.SlotName != target.DecodingName() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s names slot %s, the target's derived name is %s; a restart drops only the target's own slot", cp.Schema, cp.Table, cp.SlotName, target.DecodingName())
	}
	if cp.Phase == checkpoint.PhaseCutover {
		if err := r.refuseSwappedRestart(ctx, pool, lock, cp); err != nil {
			return err
		}
	}
	if err := r.discardShadow(ctx, pool, lock, target); err != nil {
		return err
	}
	if err := r.discardHold(ctx, pool, lock, target, cp); err != nil {
		return err
	}
	if err := r.store.Delete(ctx, lock, cp.Schema, cp.Table, cp.Identity()); err != nil {
		return fmt.Errorf("discard checkpoint row for %s.%s: %w", cp.Schema, cp.Table, err)
	}
	return nil
}

// refuseSwappedRestart refuses the restart of a cutover row whose swap the
// catalog shows committed, or whose retained source is already gone — in
// both the shadow has taken the source's name. A swap the catalog shows
// rolled back is a run back before its cutover, which the restart may
// discard.
func (r *Runner) refuseSwappedRestart(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, cp checkpoint.Checkpoint) error {
	proof, err := rowProof(cp)
	if err != nil {
		return err
	}
	// INV: LK-4 — a cutover row is answered by the catalog, never assumed.
	_, err = InspectSwapped(ctx, pool, lock, proof, r.opts.Shadow)
	switch {
	case err == nil, errors.Is(err, ErrOldTableNotFound):
		return refuse(CauseRestartSwapCommitted, nil, "checkpoint row for %s.%s is in phase %s and the catalog shows its swap committed; run the statement again without the restart to finish it", cp.Schema, cp.Table, cp.Phase)
	case errors.Is(err, ErrNotSwapped):
		return nil
	default:
		return err
	}
}

// discardUnrecorded drops a relation under the target's shadow name that
// no checkpoint row records, and takes any write fence off the source, so
// a restart starts fresh where a plain run would refuse the shadow as
// shadow-unrecorded. The fence is dropped whichever kind of run this one
// is: a quiesced leftover's fence must not outlive a restart into a live
// run, whose source keeps taking writes, and the drop is idempotent. No
// slot is touched: without a row nothing says a slot of the derived name
// is this run's to drop (ST-3), and the reaper takes an orphan of the
// engine's own.
func (r *Runner) discardUnrecorded(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget) error {
	if err := r.discardShadow(ctx, pool, lock, target); err != nil {
		return err
	}
	return DropWriteFence(ctx, pool, lock, target, r.opts.Shadow)
}

// discardShadow drops the shadow under the target's derived name; a name
// nothing bears is already discarded.
func (r *Runner) discardShadow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget) error {
	err := DropShadow(ctx, pool, lock, target, r.opts.Shadow)
	if err != nil && !errors.Is(err, ErrShadowNotFound) {
		return err
	}
	return nil
}

// discardHold releases the hold the stopped run cp kept on the source: the
// slot a live row names, which pins WAL until it is dropped, or the write
// fence a quiesced row stands for. Which of the two is read from the row,
// not from the target's proof: the row records what the stopped run made,
// and a restart discards that whichever kind of run this one is.
// discardRow has already refused a row whose slot is not the target's.
func (r *Runner) discardHold(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, cp checkpoint.Checkpoint) error {
	if cp.SlotName != "" {
		return decode.DropSlot(ctx, r.cfg, pool, cp.SlotName)
	}
	return DropWriteFence(ctx, pool, lock, target, r.opts.Shadow)
}
