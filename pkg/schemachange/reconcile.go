package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// Reconciler is reconcile mode (ST-4): the recovery for a run whose
// replication slot is gone or lost, keeping the shadow and the copier's
// watermark and replacing only what the slot carried. One Reconciler
// serves one built shadow; its verifier is built up front, and the options
// its clear of the shadow runs under are validated then, so a reconcile
// never starts with options that cannot run a pass.
type Reconciler struct {
	cfg      dbconn.Config
	lock     *dbconn.TableLockSession
	store    *checkpoint.Store
	target   preflight.CopySwapTarget
	shadow   BuiltShadow
	verifier *checksum.Verifier
	// clearOpts bounds the clear of the shadow above a partial watermark
	// as the pass's options bound the pass: the same timeouts, the pass's
	// chunk size as the batch size.
	clearOpts copier.Options
}

// Reconciled is what a completed reconcile hands back: the slot change
// capture resumes from, the checkpoint row as it now stands, and the recopy
// pass's outcome — the zero Outcome when nothing had landed to recopy.
type Reconciled struct {
	// Slot is the new slot with its replication connection still open, as
	// CreateSlot returns it; the caller opens its stream at
	// Slot.ConsistentPoint() and closes it.
	Slot *decode.Slot
	// Checkpoint is the row after the reconcile: the phase the watermark
	// implies, the new slot's name, and its consistent point as the last
	// applied position.
	Checkpoint checkpoint.Checkpoint
	// Outcome is the recopy pass's report and the chunks it recopied.
	Outcome checksum.Outcome
}

// NewReconciler prepares reconcile mode for target and its built shadow,
// under the table's lock session, writing the checkpoint row through
// store. opts bound the recopy pass as they bound any verification pass.
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
	clearOpts := clearOptions(opts)
	if err := clearOpts.Validate(); err != nil {
		return nil, err
	}
	return &Reconciler{cfg: cfg, lock: lock, store: store, target: target, shadow: shadow, verifier: verifier, clearOpts: clearOpts}, nil
}

// clearOptions bounds the reconcile's clear of the shadow as its pass is
// bounded. The copier's worker count and clock have no bearing on a clear,
// which runs one batch at a time and times nothing, and are left to their
// defaults.
func clearOptions(opts checksum.Options) copier.Options {
	return copier.Options{
		LockTimeout:      opts.LockTimeout,
		StatementTimeout: opts.StatementTimeout,
		Chunker:          opts.Chunker,
	}
}

// Reconcile recovers from slot loss for the run cp records, in an order a
// crash at any point leaves restartable from the top:
//
//  1. The source and shadow are confirmed to be the relations the built
//     shadow proved, so a row whose tables have been swapped or replaced
//     under their names is refused before anything is written.
//  2. When anything has landed, the shadow is cleared above the row's
//     watermark, as the copier that resumes from it would clear it first
//     (CO-4): the rows the earlier run's unlanded chunks committed there
//     lie outside the pass that follows, and one still holding a unique
//     value the source has since moved into a chunk the pass recopies
//     would collide with every recopy of it, with no comparison through
//     the watermark able to clear the collision. A complete watermark has
//     nothing above it. The clear runs before any slot command, so a new
//     slot retains WAL only while the pass runs; a crash after it leaves
//     the row in its earlier phase, and clearing again is harmless.
//  3. The row is saved in PhaseReconciling, naming the slot the reconcile
//     will own under the target's derived name, so a resume that finds
//     it reconciles again rather than trusting whatever slot it finds,
//     and the reaper sees the slot as the row's.
//  4. The slot under the derived name is dropped — a slot already gone is
//     success, a half-made one from an earlier attempt goes too.
//  5. A new slot is created under the same name. Every change committed
//     after its consistent point replays when the stream is reopened.
//  6. Then one recopy pass runs through the row's watermark under
//     checksum.DivergenceRecopy: every chunk that differs is recopied
//     from the source, which is where the changes the lost slot never
//     delivered are, and is not read again. The source keeps taking
//     writes throughout; a write that lands in a chunk after its recopy is
//     behind the new slot's consistent point and replays when the stream
//     reopens, so the pass has nothing to prove about it. A unique value
//     the source moves from a chunk the pass read equal into a differing
//     chunk, between the comparison and the recopy, makes the recopy
//     collide with the shadow row still holding it; the pass then compares
//     again and recopies both chunks, a bounded number of times.
//  7. The row is saved out of PhaseReconciling: PhaseCatchingUp when the
//     watermark is complete, PhaseCopying when the copier still has keys
//     to land, with the new slot's consistent point as the last applied
//     position.
//
// The lost slot is read nowhere: whether the catalog shows it gone, lost,
// or intact under the name, the row said reconcile and that is what runs.
// A row in a terminal or cutover phase, or one that does not describe
// this target and shadow, is refused. On an error the slot the attempt
// created, if any, stays under the row's name, so the reaper leaves it for
// the next attempt to drop, and the row stays in PhaseReconciling for that
// attempt. The clear can end an attempt before any slot command, with the
// copier's own refusals (a lost lock, a replaced relation, conversion
// drift) or with an untyped lock timeout when a writer still holds the
// shadow past lock_timeout; either leaves the row and the watermark as
// they were, and a later attempt clears again. Retry the timeout.
//
// The whole sequence runs under the lock session's context: a lock lost
// at any point cancels the statement in flight — a drop still waiting out
// the lost slot's holder included — before the next slot command is
// issued, and is reported as shadow-lock-lost, so a session that has lost
// the table never drops, or makes, a slot that a run holding the lock
// since may own.
func (r *Reconciler) Reconcile(ctx context.Context, pool *pgxpool.Pool, cp checkpoint.Checkpoint) (Reconciled, error) {
	if err := checkResumeRow(r.target, cp); err != nil {
		return Reconciled{}, err
	}
	if err := r.checkShadowRow(cp); err != nil {
		return Reconciled{}, err
	}
	if err := requireTableLock(r.lock, r.target.Schema(), r.target.Table()); err != nil {
		return Reconciled{}, err
	}
	// INV: LK-1 — every slot command below ends with the lock.
	ctx, stop := r.lock.Bind(ctx)
	defer stop()
	reconciled, err := r.reconcile(ctx, pool, cp)
	if err != nil {
		return Reconciled{}, lockLossCause(r.lock, fmt.Errorf("reconcile %s.%s: %w", cp.Schema, cp.Table, err))
	}
	return reconciled, nil
}

func (r *Reconciler) reconcile(ctx context.Context, pool *pgxpool.Pool, cp checkpoint.Checkpoint) (Reconciled, error) {
	if err := r.confirmShadowRelations(ctx, pool); err != nil {
		return Reconciled{}, err
	}
	// The clear needs only the lock and the shadow, so it runs before any
	// slot command: a new slot then retains WAL only while the pass runs,
	// and a clear that fails ends the attempt with no slot made.
	if err := r.clearAbove(ctx, pool, cp); err != nil {
		return Reconciled{}, err
	}
	// INV: ST-4 — the durable marker precedes every change to the slot.
	name := r.target.DecodingName()
	marked := cp
	marked.Phase = checkpoint.PhaseReconciling
	marked.SlotName = name
	marked.PublicationName = name
	slot, err := r.claimSlot(ctx, pool, marked)
	if err != nil {
		return Reconciled{}, err
	}
	outcome, err := r.recopy(ctx, pool, cp)
	if err != nil {
		return Reconciled{}, errors.Join(err, slot.Close(context.WithoutCancel(ctx)))
	}
	resumed := marked
	resumed.Phase = phaseAfterReconcile(cp)
	resumed.LastAppliedLSN = slot.ConsistentPoint()
	if err := r.store.Save(ctx, r.lock, resumed); err != nil {
		return Reconciled{}, errors.Join(err, slot.Close(context.WithoutCancel(ctx)))
	}
	return Reconciled{Slot: slot, Checkpoint: resumed, Outcome: outcome}, nil
}

// claimSlot writes the row that names the slot and replaces whatever slot
// bears the name with a fresh one, all under the engine's lock on the
// slot's name, so the reaper never reads the row's old phase and drops a
// slot the committed row owns (ST-3). The publication stays in place: the
// source is live through a reconcile, and its writers look the publication
// up as they run. The new slot's connection is the caller's to close.
func (r *Reconciler) claimSlot(ctx context.Context, pool *pgxpool.Pool, marked checkpoint.Checkpoint) (*decode.Slot, error) {
	var slot *decode.Slot
	err := r.store.WithSlotLock(ctx, r.cfg, marked.SlotName, func(ctx context.Context) error {
		if err := r.store.Save(ctx, r.lock, marked); err != nil {
			return err
		}
		created, err := decode.ReplaceSlot(ctx, r.cfg, pool, r.target)
		if err != nil {
			return err
		}
		slot = created
		return nil
	})
	if err != nil && slot != nil {
		// The slot was created but the lock was not let go cleanly: the
		// slot stays for the resume path, its connection does not.
		return nil, errors.Join(err, slot.Close(context.WithoutCancel(ctx)))
	}
	if err != nil {
		return nil, err
	}
	return slot, nil
}

// confirmShadowRelations refuses a source or shadow whose OID moved since
// the build, read from a transaction that has confirmed the table lock:
// a committed swap leaves the shadow's name on the relation that was the
// source, and a recopy into it would overwrite live data.
func (r *Reconciler) confirmShadowRelations(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("confirm relations: %w", err)
	}
	defer func() {
		// Redundant safety closer: a read-only transaction that is
		// rolled back below either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := confirmTableLock(ctx, tx, r.lock); err != nil {
		return err
	}
	if err := confirmRelations(ctx, tx, r.shadow); err != nil {
		return err
	}
	if err := tx.Rollback(ctx); err != nil {
		return fmt.Errorf("confirm relations: %w", err)
	}
	return nil
}

// clearAbove clears the shadow above the row's watermark, or does nothing
// when nothing has landed: with no watermark there is no pass, so no row
// above one to collide with, and whatever the earlier run committed is the
// resumed copier's to clear.
func (r *Reconciler) clearAbove(ctx context.Context, pool *pgxpool.Pool, cp checkpoint.Checkpoint) error {
	if !cp.Watermark.Valid() {
		return nil
	}
	// INV: CO-4 — every shadow row the pass does not compare is removed
	// before the pass, so none can hold a value a recopy puts back.
	return copier.ClearAbove(ctx, pool, r.target, r.shadow, r.lock, cp.Watermark, r.clearOpts)
}

// recopy runs the recopy pass through the row's watermark, or nothing
// when nothing has landed.
func (r *Reconciler) recopy(ctx context.Context, pool *pgxpool.Pool, cp checkpoint.Checkpoint) (checksum.Outcome, error) {
	if !cp.Watermark.Valid() {
		return checksum.Outcome{}, nil
	}
	// INV: CO-3 — the policy is stated: reconcile mode recopies.
	return r.verifier.Check(ctx, pool, cp.Watermark, checksum.DivergenceRecopy)
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
// the one this Reconciler was built for: the recopy pass would then
// write into a table the row does not describe.
func (r *Reconciler) checkShadowRow(cp checkpoint.Checkpoint) error {
	if cp.ShadowTable != r.shadow.ShadowTable() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s names shadow %s, the built shadow is %s", cp.Schema, cp.Table, cp.ShadowTable, r.shadow.ShadowTable())
	}
	if cp.SourceFingerprint != r.shadow.SourceFingerprint() || cp.TargetFingerprint != r.shadow.TargetFingerprint() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s carries fingerprints of another statement than the built shadow", cp.Schema, cp.Table)
	}
	return nil
}
