package schemachange

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// defaultCheckpointInterval is how often a copy in progress writes its
// landed watermark to the checkpoint row when RunOptions leaves the
// interval zero.
const defaultCheckpointInterval = 10 * time.Second

// RunOptions bounds one quiesced copy-and-swap run. Zero values take the
// defaults of each step they configure.
type RunOptions struct {
	// Shadow bounds the shadow build, inspection, cutover gate, swapped
	// table inspection, and old table drop.
	Shadow Options
	// Copier bounds the copy.
	Copier copier.Options
	// Checksum bounds the verification pass.
	Checksum checksum.Options
	// Cutover bounds the swap and its ACCESS EXCLUSIVE acquisition.
	Cutover CutoverOptions
	// KeepOld leaves the retained source under its derived _old name after
	// the swap (D9), for a rollback window the caller manages; the run
	// still reaches PhaseDone, with the write fence taken off the old
	// table so it can be renamed back and written. False drops it before
	// the row is done. The caller ends the window with InspectSwapped on
	// the done row's proof and DropOldTable, under the table's lock.
	KeepOld bool
	// CheckpointInterval is how often the copy in progress saves its
	// landed watermark, so a run that stops mid-copy resumes from the last
	// save rather than from the start (TM-3). Zero means the default.
	CheckpointInterval time.Duration
	// Sleep waits between periodic saves; nil waits on the wall clock.
	// Tests inject one to drive the saves without real time passing.
	Sleep func(ctx context.Context, d time.Duration) error
	// AfterCheckpoint, when set, is called after every successful save of
	// the checkpoint row with the row as saved, and is the run's one
	// progress signal: the phase saves call it on the caller's goroutine,
	// the periodic watermark saves on the copy's saving goroutine, so it
	// must be safe to call from either, and the periodic calls arrive at
	// CheckpointInterval, not as the copy lands rows. An error it returns
	// stops the run there, with the row as the last save left it, so a
	// test can stop a run at any transition and resume it (TM-3 fault
	// hook).
	AfterCheckpoint func(ctx context.Context, cp checkpoint.Checkpoint) error
}

// validate refuses options a run could not honour. The copier's and the
// verifier's options are validated where they are consumed, by their own
// constructors; the shadow and cutover options here, since every phase
// uses them.
func (o RunOptions) validate() error {
	if err := o.Shadow.validate(); err != nil {
		return err
	}
	if err := o.Cutover.validate(); err != nil {
		return err
	}
	if o.CheckpointInterval < 0 {
		return fmt.Errorf("%w: checkpoint interval %s is negative; use zero for the default", ErrInvalidOptions, o.CheckpointInterval)
	}
	return nil
}

func (o RunOptions) checkpointInterval() time.Duration {
	if o.CheckpointInterval == 0 {
		return defaultCheckpointInterval
	}
	return o.CheckpointInterval
}

func (o RunOptions) sleep() func(context.Context, time.Duration) error {
	if o.Sleep == nil {
		return sleepContext
	}
	return o.Sleep
}

// Runner drives one quiesced copy-and-swap change end to end: table lock,
// shadow build, copy, verification pass, cutover gate, swap, and old table
// drop, with the checkpoint row written at every transition so a run that
// stops anywhere resumes from the row (TM-3). Quiesced means no writer
// touches the source while the run is in progress: the Runner decodes no
// WAL and applies no captured change, so the copy and the verification
// pass are the whole proof of data equality (CO-1). The Runner enforces
// that itself with a write fence on the source — a statement trigger that
// refuses INSERT, UPDATE, DELETE, and TRUNCATE with SQLSTATE
// WriteFenceSQLState — installed before the copy starts and in place until
// the swap, across any stop and resume in between; a writer that reaches
// the table gets an error, never a silent loss. A target proven for a run
// that decodes WAL is refused; that path has its own orchestration.
type Runner struct {
	cfg   dbconn.Config
	store *checkpoint.Store
	opts  RunOptions
}

// Result is what a completed run hands back.
type Result struct {
	// Checkpoint is the row as the run saved it last, in PhaseDone. It is
	// the row as written, not as read back: UpdatedAt, which the store
	// stamps from its own clock at the write, is zero here and is read
	// with Store.Inspect.
	Checkpoint checkpoint.Checkpoint
	// Shadow is the proof of the shadow the run built or resumed on.
	Shadow Proof
	// Swapped is the proof of the committed swap, made by this run's
	// cutover or re-derived from the catalog on resume. It is the zero
	// SwapProof when the run resumed after the retained source was already
	// dropped: the renames the swap performed cannot be read back once
	// one side of them is gone, and Shadow still names the live table.
	Swapped SwapProof
	// Verification is the report of the verification pass this run made,
	// or the zero Report when the run resumed after the swap had already
	// committed and so made none.
	Verification checksum.Report
	// OldTableDropped reports whether the retained source is gone when the
	// run returns: dropped by this run, or found already dropped by the
	// run that was resumed. It is false under KeepOld.
	OldTableDropped bool
}

// NewRunner prepares runs over the database cfg names, writing the
// checkpoint row through store. It refuses a nil store and options a run
// could not honour. cfg opens the dedicated connection the table lock
// session holds for the run; every other statement runs on the pool
// handed to Run, and store writes through its own pool. All three must
// name one database: each working transaction confirms the table lock
// from the pool it runs on, and a pool that reaches another database sees
// no lock and refuses as shadow-lock-unconfirmed before writing anything.
func NewRunner(cfg dbconn.Config, store *checkpoint.Store, opts RunOptions) (*Runner, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: a run requires a checkpoint store", ErrInvariantViolation)
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, store: store, opts: opts}, nil
}

// Run carries target through the gated statement st, or resumes the run
// its checkpoint row records, and returns once the change is complete and
// the row is in PhaseDone. It takes the table's lock for the whole run
// (LK-1), makes sure the checkpoint table exists, reads the target's row,
// and dispatches on what it finds:
//
//   - No row: the shadow is built from st, the write fence put on the
//     source, and the row saved in PhaseCopying, with st's text. A
//     relation already under the shadow's name is refused as
//     shadow-unrecorded, since no row says what built it (ST-2). A first
//     save whose row does not land drops the shadow and the fence again,
//     so the next Run starts fresh rather than meeting that refusal.
//   - Any other row records a run of the statement it carries, and st
//     must be that statement — the same operations on the same table,
//     compared as parsed — or the row is refused as resume-row-mismatch
//     before anything is touched: a resume completes the change the row
//     records, never the one the caller changed its mind to.
//   - A row in PhaseCopying or PhaseVerifying: the shadow under the row's
//     name is inspected and its proof must equal the one the row carries,
//     or the row is refused as resume-row-mismatch; the run continues
//     from the row's phase and watermark.
//   - A row in PhaseCutover: whether the swap committed is read from the
//     catalog (LK-4) — a committed swap continues at the old table drop,
//     a swap that rolled back is verified and gated again and swapped.
//   - A terminal row is refused as resume-row-terminal: the run it
//     records has ended, and a caller that means to run the table again
//     deletes it first with Store.Delete, under the table's lock.
//   - A row naming a slot, or in a phase only a run that decodes WAL
//     enters, is refused as resume-row-decoding.
//
// The phases then run in order — the write fence on the source, installed
// over the one a stopped run left; copy, saving the landed watermark every
// CheckpointInterval and when the copy ends; one verification pass through the complete watermark under
// checksum.DivergenceAbort; the cutover gate; the row saved in
// PhaseCutover; the swap; the old table drop, or under KeepOld the fence
// taken off the old table; the row saved in PhaseDone. The row is never
// written to PhaseFailed: a run that stops leaves the row where its last
// save put it, with the fence in place, and the next Run resumes from
// there. A run that stopped under a verification difference is wedged in
// PhaseVerifying until the operator finds the writer, drops the shadow
// with DropShadow, takes the fence off with DropWriteFence, and deletes
// the row; no Run goes back to the copy on its own. Every error is
// returned as the step returned it, with a lost table lock reported as
// shadow-lock-lost.
func (r *Runner) Run(ctx context.Context, pool *pgxpool.Pool, target preflight.CopySwapTarget, st statement.Statement) (result Result, err error) {
	if err := checkProof(target); err != nil {
		return Result{}, err
	}
	if target.DecodesWAL() {
		return Result{}, fmt.Errorf("%w: copy-and-swap proof for %s.%s was verified for a run that decodes WAL; the quiesced runner decodes none", ErrInvariantViolation, target.Schema(), target.Table())
	}
	lock, err := dbconn.AcquireTableLock(ctx, r.cfg, target.Schema(), target.Table())
	if err != nil {
		return Result{}, err
	}
	defer func() {
		// A session that lost the lock closes itself; releasing it would
		// only report that the lock was not held. The release outlives the
		// caller's context, since the lock is held for the run and not for
		// the call, but is bounded so a cancelled run against a server that
		// has stopped answering still returns.
		if lock.Err() == nil {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
			defer cancel()
			err = errors.Join(err, lock.Release(releaseCtx))
		}
	}()
	result, err = r.run(ctx, pool, lock, target, st)
	if err != nil {
		return Result{}, lockLossCause(lock, fmt.Errorf("run %s.%s: %w", target.Schema(), target.Table(), err))
	}
	return result, nil
}

func (r *Runner) run(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, st statement.Statement) (Result, error) {
	if err := r.store.Ensure(ctx); err != nil {
		return Result{}, err
	}
	state, err := r.dispatch(ctx, pool, lock, target, st)
	if err != nil {
		return Result{}, err
	}
	result := Result{Shadow: state.proof}
	if !state.swapped {
		if state.cp.Phase == checkpoint.PhaseCopying {
			if err := r.copy(ctx, pool, lock, target, &state); err != nil {
				return Result{}, err
			}
		}
		verified, report, err := r.verify(ctx, pool, lock, target, state)
		result.Verification = report
		if err != nil {
			return Result{}, err
		}
		ready, err := GateCutover(ctx, pool, lock, state.built, verified, r.opts.Shadow)
		if err != nil {
			return Result{}, err
		}
		if state.cp.Phase != checkpoint.PhaseCutover {
			// INV: LK-4 — the row says cutover before the swap is attempted.
			if err := r.save(ctx, lock, &state, checkpoint.PhaseCutover); err != nil {
				return Result{}, err
			}
		}
		swapped, err := Cutover(ctx, pool, lock, ready, nil, r.opts.Cutover)
		if err != nil {
			return Result{}, err
		}
		state.swappedTable = swapped
		state.swapped = true
	}
	if !state.oldTableGone {
		// A swap found with its retained source already dropped has no
		// SwappedTable to report; Result.Swapped stays zero.
		result.Swapped = state.swappedTable.Proof()
	}
	if !state.oldTableGone {
		if err := r.settleOldTable(ctx, pool, lock, &state); err != nil {
			return Result{}, err
		}
	}
	result.OldTableDropped = state.oldTableGone
	if err := r.save(ctx, lock, &state, checkpoint.PhaseDone); err != nil {
		return Result{}, err
	}
	// The row as saved is the row the run leaves; reading it back could
	// only fail a change that is complete and recorded.
	result.Checkpoint = state.cp
	return result, nil
}

// settleOldTable finishes with the retained source after the swap: drops
// it, or under KeepOld takes the write fence off it, since the fence rode
// the rename and a table kept for a rollback window has to take writes
// the moment it is renamed back.
func (r *Runner) settleOldTable(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, state *runState) error {
	if r.opts.KeepOld {
		return dropWriteFenceFromOld(ctx, pool, lock, state.swappedTable, r.opts.Shadow)
	}
	err := DropOldTable(ctx, pool, lock, state.swappedTable, r.opts.Shadow)
	if err != nil && !errors.Is(err, ErrOldTableNotFound) {
		return err
	}
	state.oldTableGone = true
	return nil
}

// verify runs the one verification pass through the row's complete
// watermark and returns the proof it minted with its report. A row that
// reaches verification short of the complete watermark is refused: the
// copy saved it, so the copy ended short of the key space.
func (r *Runner) verify(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, state runState) (checksum.VerifiedShadow, checksum.Report, error) {
	if !state.cp.Watermark.Complete() {
		// INV: CO-4
		return checksum.VerifiedShadow{}, checksum.Report{}, fmt.Errorf("%w (CO-4): checkpoint row for %s.%s is in phase %s with a watermark short of the key space", ErrInvariantViolation, state.cp.Schema, state.cp.Table, state.cp.Phase)
	}
	verifier, err := checksum.NewVerifier(target, state.built, lock, r.opts.Checksum)
	if err != nil {
		return checksum.VerifiedShadow{}, checksum.Report{}, err
	}
	// INV: CO-3 — the policy is stated: a quiesced run has no writer to
	// explain a difference, so a difference stops it.
	outcome, err := verifier.Check(ctx, pool, state.cp.Watermark, checksum.DivergenceAbort)
	if err != nil {
		return checksum.VerifiedShadow{}, outcome.Report, err
	}
	verified, minted := outcome.VerifiedShadow()
	if !minted {
		// INV: CO-1
		return checksum.VerifiedShadow{}, outcome.Report, fmt.Errorf("%w (CO-1): a clean pass through the complete watermark of %s.%s minted no verified shadow", ErrInvariantViolation, state.cp.Schema, state.cp.Table)
	}
	return verified, outcome.Report, nil
}

// save writes the row in phase, with the watermark the state carries, and
// hands the row as saved to AfterCheckpoint. The state's row is the row as
// saved only once both have succeeded.
func (r *Runner) save(ctx context.Context, lock *dbconn.TableLockSession, state *runState, phase checkpoint.Phase) error {
	cp := state.cp
	cp.Phase = phase
	if err := r.store.Save(ctx, lock, cp); err != nil {
		return err
	}
	state.cp = cp
	return r.afterCheckpoint(ctx, cp)
}

func (r *Runner) afterCheckpoint(ctx context.Context, cp checkpoint.Checkpoint) error {
	if r.opts.AfterCheckpoint == nil {
		return nil
	}
	if err := r.opts.AfterCheckpoint(ctx, cp); err != nil {
		return fmt.Errorf("after the checkpoint in phase %s: %w", cp.Phase, err)
	}
	return nil
}
