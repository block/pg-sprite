package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// Reconciler is reconcile mode (ST-4): the recovery for a run whose
// replication slot is gone or lost, keeping the shadow and the copier's
// watermark and replacing only what the slot carried. One Reconciler
// serves one built shadow; its verifier is built up front so a reconcile
// never starts with options that cannot run a pass.
type Reconciler struct {
	cfg      dbconn.Config
	lock     *dbconn.TableLockSession
	store    *checkpoint.Store
	target   preflight.CopySwapTarget
	shadow   BuiltShadow
	verifier *checksum.Verifier
}

// Reconciled is what a completed reconcile hands back: the slot change
// capture resumes from, the checkpoint row as it now stands, and the repair
// pass's outcome — the zero Outcome when nothing had landed to repair.
type Reconciled struct {
	// Slot is the new slot with its replication connection still open, as
	// CreateSlot returns it; the caller opens its stream at
	// Slot.ConsistentPoint() and closes it.
	Slot *decode.Slot
	// Checkpoint is the row after the reconcile: the phase the watermark
	// implies, the new slot's name, and its consistent point as the last
	// applied position.
	Checkpoint checkpoint.Checkpoint
	// Outcome is the repair pass's report and repairs.
	Outcome checksum.Outcome
}

// NewReconciler prepares reconcile mode for target and its built shadow,
// under the table's lock session, writing the checkpoint row through
// store. opts bound the repair pass as they bound any verification pass.
func NewReconciler(cfg dbconn.Config, lock *dbconn.TableLockSession, store *checkpoint.Store, target preflight.CopySwapTarget, shadow BuiltShadow, opts checksum.Options) (*Reconciler, error) {
	if err := checkProof(target); err != nil {
		return nil, err
	}
	if !target.DecodesWAL() {
		// INV: ST-4 — a run without a slot has no slot to lose.
		return nil, fmt.Errorf("%w (ST-4): copy-and-swap proof for %s.%s was not verified for a run that decodes WAL", ErrInvariantViolation, target.Schema(), target.Table())
	}
	if err := requireTableLock(lock, target.Schema(), target.Table()); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("%w: reconcile requires a checkpoint store", ErrInvariantViolation)
	}
	verifier, err := checksum.NewVerifier(target, shadow, lock, opts)
	if err != nil {
		return nil, err
	}
	return &Reconciler{cfg: cfg, lock: lock, store: store, target: target, shadow: shadow, verifier: verifier}, nil
}

// Reconcile recovers from slot loss for the run cp records, in an order a
// crash at any point leaves restartable from the top:
//
//  1. The row is saved in PhaseReconciling, so a resume that finds it
//     reconciles again rather than trusting whatever slot it finds.
//  2. The slot under the target's derived name is dropped — a slot already
//     gone is success, a half-made one from an earlier attempt goes too.
//  3. A new slot is created under the same name. Every change committed
//     after its consistent point replays when the stream is reopened.
//  4. When anything has landed, one repair pass runs through the row's
//     watermark under checksum.DivergenceRepair: every chunk that differs
//     is recopied from the source, which is where the changes the lost
//     slot never delivered are. The pass assumes nothing else writes the
//     shadow; a chunk still different when read again is a
//     *checksum.RepairError, and the row stays in PhaseReconciling for
//     the next attempt. A change that lands between the consistent point
//     and the pass is repaired now and replayed later, which the flush
//     absorbs.
//  5. The row is saved out of PhaseReconciling: PhaseCatchingUp when the
//     watermark is complete, PhaseCopying when the copier still has keys
//     to land, naming the new slot with its consistent point as the last
//     applied position.
//
// The lost slot is read nowhere: whether the catalog shows it gone, lost,
// or intact under the name, the row said reconcile and that is what runs.
// A row in a terminal phase, or one that does not describe this target
// and shadow, is refused. On an error the slot the attempt created, if
// any, stays under the row's name, so the reaper leaves it for the next
// attempt to drop.
func (r *Reconciler) Reconcile(ctx context.Context, pool *pgxpool.Pool, cp checkpoint.Checkpoint) (Reconciled, error) {
	if err := checkResumeRow(r.target, cp); err != nil {
		return Reconciled{}, err
	}
	if err := r.checkShadowRow(cp); err != nil {
		return Reconciled{}, err
	}
	reconciled, err := r.reconcile(ctx, pool, cp)
	if err != nil {
		return Reconciled{}, lockLossCause(r.lock, fmt.Errorf("reconcile %s.%s: %w", cp.Schema, cp.Table, err))
	}
	return reconciled, nil
}

func (r *Reconciler) reconcile(ctx context.Context, pool *pgxpool.Pool, cp checkpoint.Checkpoint) (Reconciled, error) {
	// INV: ST-4 — the durable marker precedes every change to the slot.
	marked := cp
	marked.Phase = checkpoint.PhaseReconciling
	if err := r.store.Save(ctx, r.lock, marked); err != nil {
		return Reconciled{}, err
	}
	name := r.target.DecodingName()
	if err := decode.DropSlot(ctx, r.cfg, pool, name); err != nil {
		return Reconciled{}, err
	}
	slot, err := decode.CreateSlot(ctx, r.cfg, pool, r.target)
	if err != nil {
		return Reconciled{}, err
	}
	outcome, err := r.repair(ctx, pool, cp)
	if err != nil {
		return Reconciled{}, errors.Join(err, slot.Close(context.WithoutCancel(ctx)))
	}
	resumed := marked
	resumed.Phase = phaseAfterReconcile(cp)
	resumed.SlotName = slot.Name()
	resumed.PublicationName = slot.Name()
	resumed.LastAppliedLSN = slot.ConsistentPoint()
	if err := r.store.Save(ctx, r.lock, resumed); err != nil {
		return Reconciled{}, errors.Join(err, slot.Close(context.WithoutCancel(ctx)))
	}
	return Reconciled{Slot: slot, Checkpoint: resumed, Outcome: outcome}, nil
}

// repair runs the repair pass through the row's watermark, or no pass
// when nothing has landed: a shadow with no rows has nothing the lost
// slot could have left stale.
func (r *Reconciler) repair(ctx context.Context, pool *pgxpool.Pool, cp checkpoint.Checkpoint) (checksum.Outcome, error) {
	if !cp.Watermark.Valid() {
		return checksum.Outcome{}, nil
	}
	// INV: CO-3 — the policy is stated: reconcile mode repairs.
	return r.verifier.Check(ctx, pool, cp.Watermark, checksum.DivergenceRepair)
}

// phaseAfterReconcile is the phase a reconciled row resumes in, read from
// the watermark rather than from the phase the row was in when the slot
// was lost: a complete watermark has every key landed, so the copier is
// done and the run catches up; anything less leaves keys to copy.
func phaseAfterReconcile(cp checkpoint.Checkpoint) checkpoint.Phase {
	if cp.Watermark.Complete() {
		return checkpoint.PhaseCatchingUp
	}
	return checkpoint.PhaseCopying
}

// checkShadowRow refuses a row that records a run on another shadow than
// the one this Reconciler was built for: the repair pass would then
// recopy into a table the row does not describe.
func (r *Reconciler) checkShadowRow(cp checkpoint.Checkpoint) error {
	if cp.ShadowTable != r.shadow.ShadowTable() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s names shadow %s, the built shadow is %s", cp.Schema, cp.Table, cp.ShadowTable, r.shadow.ShadowTable())
	}
	if cp.SourceFingerprint != r.shadow.SourceFingerprint() || cp.TargetFingerprint != r.shadow.TargetFingerprint() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s carries fingerprints of another statement than the built shadow", cp.Schema, cp.Table)
	}
	return nil
}
