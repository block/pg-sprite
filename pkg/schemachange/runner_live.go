package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// swapLive fills and swaps the shadow of a live run, with the source
// taking writes throughout. The row's slot is made decodable from, the
// stream on it is opened at the row's position and applied to the shadow
// by a capture that runs beside the fill — the copy, when the row is
// still in it, then catching up to a marker past every change committed
// so far, then the live verification pass — and the cutover gate. The
// capture is ended before the swap, so the swap's drain is the only
// stream on the slot, and the row records the position the capture
// confirmed through, which is where the drain reopens the stream under
// the swap's lock (ST-4).
//
// A row the run stopped in PhaseCutover whose swap the catalog shows
// rolled back is a run back before its cutover: the row is written back
// to PhaseCatchingUp first, since the resume inspection and the
// reconcile read a cutover row as a swap whose outcome is still unknown
// (LK-4), and the run then catches up and verifies again from the row's
// position.
func (r *Runner) swapLive(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, state *runState) (checksum.Report, error) {
	if state.cp.Phase == checkpoint.PhaseCutover {
		if err := r.save(ctx, lock, state, checkpoint.PhaseCatchingUp); err != nil {
			return checksum.Report{}, err
		}
	}
	if err := r.liveReady(ctx, pool, lock, target, state); err != nil {
		return checksum.Report{}, err
	}
	from := state.cp.LastAppliedLSN
	if from == 0 {
		// INV: ST-4 — a live row always carries the position its stream
		// resumes from; a row without one was not written by this run.
		return checksum.Report{}, fmt.Errorf("%w (ST-4): checkpoint row for %s.%s names slot %s but no position to resume its stream from", ErrInvariantViolation, state.cp.Schema, state.cp.Table, state.cp.SlotName)
	}
	flusher, err := applier.NewFlusher(target, state.built, lock, r.opts.Applier)
	if err != nil {
		return checksum.Report{}, err
	}
	fill, err := r.newLiveFill(target, lock, state)
	if err != nil {
		return checksum.Report{}, err
	}
	cap, liveCtx, err := r.startCapture(ctx, pool, target, flusher, fill.positions, from)
	if err != nil {
		return checksum.Report{}, err
	}
	ready, report, fillErr := r.fillLive(liveCtx, pool, lock, target, state, fill, cap)
	endErr := cap.end(ctx, pool, r.opts.sleep())
	if err := captureOutcome(cap, fillErr, endErr); err != nil {
		return report, err
	}
	// INV: ST-4 — the position the drain reopens from is the one the
	// capture confirmed: everything below it is in the shadow.
	state.cp.LastAppliedLSN = max(from, cap.catchup.Confirmed())
	// INV: LK-4 — the row says cutover before the swap is attempted.
	if err := r.save(ctx, lock, state, checkpoint.PhaseCutover); err != nil {
		return report, err
	}
	drain := r.drain(target, state.built, flusher, state.cp.Watermark, state.cp.LastAppliedLSN)
	swapped, err := Cutover(ctx, pool, lock, ready, drain, r.opts.Cutover)
	if err != nil {
		return report, err
	}
	state.swappedTable = swapped
	state.swapped = true
	return report, nil
}

// liveFill is what a live run fills the shadow with while its capture
// runs: the copier, when the row is still copying, and in either case the
// position source the catch-up reads for the keys it may write (CO-4).
type liveFill struct {
	copier    *copier.Copier
	positions applier.PositionSource
}

// newLiveFill makes the fill for the row's phase. A row past the copy
// carries the complete watermark, so every key is landed and the catch-up
// may write any of them; a row still copying carries the watermark the
// copier resumes from, and the copier's own position is what landed.
func (r *Runner) newLiveFill(target preflight.CopySwapTarget, lock *dbconn.TableLockSession, state *runState) (liveFill, error) {
	if state.cp.Phase == checkpoint.PhaseCopying {
		c, err := copier.NewCopier(target, state.built, lock, state.cp.Watermark, r.opts.Copier)
		if err != nil {
			return liveFill{}, copyRefusal(err, state.cp.Schema, state.cp.Table)
		}
		return liveFill{copier: c, positions: c}, nil
	}
	if !state.cp.Watermark.Complete() {
		// INV: CO-4
		return liveFill{}, fmt.Errorf("%w (CO-4): checkpoint row for %s.%s is in phase %s with a watermark short of the key space", ErrInvariantViolation, state.cp.Schema, state.cp.Table, state.cp.Phase)
	}
	return liveFill{positions: landedPosition{watermark: state.cp.Watermark}}, nil
}

// landedPosition is the position source of a run whose copy has landed
// every key: the complete watermark, cut and landed alike, with nothing in
// flight.
type landedPosition struct {
	watermark copier.Watermark
}

// Position implements applier.PositionSource.
func (p landedPosition) Position() copier.Position {
	return copier.Position{Watermark: p.watermark, Cut: p.watermark}
}

// fillLive carries the row from its phase to the cutover gate under the
// running capture: the copy, catching up to a marker, the live
// verification pass, the gate. Every row before the gate catches up
// before the pass, whichever phase it resumed in. ctx is the capture's context, so a
// capture that fails ends the step in progress.
func (r *Runner) fillLive(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, state *runState, fill liveFill, cap *capture) (CutoverReady, checksum.Report, error) {
	if state.cp.Phase == checkpoint.PhaseCopying {
		if err := r.runCopy(ctx, pool, lock, fill.copier, state, cap.catchup.Confirmed, checkpoint.PhaseCatchingUp); err != nil {
			return CutoverReady{}, checksum.Report{}, err
		}
	}
	// A row resumed in PhaseVerifying catches up again before the pass:
	// the pass's settle wait bounds how far the stream lags a chunk's
	// read, not the backlog a stopped run left, and a pass that starts
	// behind it leaves every chunk unsettled.
	if state.cp.Phase == checkpoint.PhaseCatchingUp || state.cp.Phase == checkpoint.PhaseVerifying {
		if err := r.awaitCaughtUp(ctx, pool, lock, state, cap); err != nil {
			return CutoverReady{}, checksum.Report{}, err
		}
	}
	verified, report, err := r.verifyWith(ctx, lock, target, *state, func(ctx context.Context, verifier *checksum.Verifier) (checksum.Outcome, error) {
		live := r.opts.Live
		live.Confirmed = cap.catchup
		// INV: CO-3 — the policy is stated: a difference the stream
		// cannot explain within the settle wait stops the run, since the
		// stream is the shadow's only other writer.
		return verifier.CheckLive(ctx, pool, state.cp.Watermark, checksum.DivergenceAbort, live)
	})
	if err != nil {
		return CutoverReady{}, report, err
	}
	ready, err := GateCutover(ctx, pool, lock, state.built, verified, r.opts.Shadow)
	if err != nil {
		return CutoverReady{}, report, err
	}
	return ready, report, nil
}

// awaitCaughtUp waits until the capture has confirmed past a marker
// written to the WAL now — every change committed before the wait began
// is then in the shadow — and leaves the row in PhaseVerifying at the
// confirmed position. The wait is bounded by ctx alone: a source written
// faster than the capture applies is a run that has not caught up, and
// the capture's own lag ceiling is what ends it.
func (r *Runner) awaitCaughtUp(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, state *runState, cap *capture) error {
	marker, err := markWAL(ctx, pool)
	if err != nil {
		return err
	}
	sleep := r.opts.sleep()
	for cap.catchup.Confirmed() < marker {
		if err := sleep(ctx, r.opts.livePoll()); err != nil {
			return fmt.Errorf("catch up %s.%s to %s: %w", state.cp.Schema, state.cp.Table, marker, err)
		}
	}
	state.cp.LastAppliedLSN = max(state.cp.LastAppliedLSN, cap.catchup.Confirmed())
	return r.save(ctx, lock, state, checkpoint.PhaseVerifying)
}
