package schemachange

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// copy runs a quiesced run's copier from the row's watermark to the end
// of the key space and leaves the row in PhaseVerifying with the complete
// watermark (TM-3).
func (r *Runner) copy(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, state *runState) error {
	c, err := copier.NewCopier(target, state.built, lock, state.cp.Watermark, r.opts.Copier)
	if err != nil {
		return copyRefusal(err, state.cp.Schema, state.cp.Table)
	}
	return r.runCopy(ctx, pool, lock, c, state, nil, checkpoint.PhaseVerifying)
}

// appliedSource reads the position the stream has been applied through,
// for the row to carry beside the watermark; nil in a quiesced run, which
// applies no stream.
type appliedSource func() decode.LSN

// runCopy runs c to the end of the key space, saving the landed watermark
// every checkpoint interval while the copy is in progress and once more
// when it ends, and leaves the row in phase next with the complete
// watermark.
//
// The periodic save runs beside the copier and stops it when a save
// fails: a run whose row cannot be written has nothing to resume from
// past the last save, so it stops rather than copy on unrecorded. The
// saved watermark only ever reads what has landed, so a row written
// mid-copy and resumed re-copies at most the chunks that were in flight.
// The applied position is read before the watermark, so a stream
// reopened at the row's position replays every change confirmed since the
// keys the row calls landed were landing (ST-4).
func (r *Runner) runCopy(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, c *copier.Copier, state *runState, applied appliedSource, next checkpoint.Phase) error {
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	saver := &watermarkSaver{runner: r, lock: lock, state: state, applied: applied}
	var wg sync.WaitGroup
	wg.Go(func() {
		saver.err = saver.loop(runCtx, c)
		if saver.err != nil {
			stop(saver.err)
		}
	})
	runErr := c.Run(runCtx, pool)
	stop(nil)
	wg.Wait()

	if saver.err != nil {
		return copyStopped(runErr, saver.err, state.cp.Schema, state.cp.Table)
	}
	// The copier has returned, so its position is final: save what landed
	// even when the copy stopped, so the resume starts from there. The
	// save outlives the caller's context since the row records work done,
	// but is bounded so a cancelled run against a server that has stopped
	// answering still returns.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
	defer cancel()
	if err := saver.saveIfMoved(saveCtx, c); err != nil {
		return errors.Join(copyRefusal(runErr, state.cp.Schema, state.cp.Table), err)
	}
	if runErr != nil {
		return copyRefusal(runErr, state.cp.Schema, state.cp.Table)
	}
	if !state.cp.Watermark.Complete() {
		// INV: CO-4
		return refuse(CauseCopyFrontierBroken, nil, "copy of %s.%s returned without error at watermark short of the key space", state.cp.Schema, state.cp.Table)
	}
	return r.save(ctx, lock, state, next)
}

// watermarkSaver writes the copier's landed watermark to the checkpoint
// row while the copy is in progress. It is used from the saving goroutine
// alone until that goroutine has been waited for, then from the caller.
type watermarkSaver struct {
	runner  *Runner
	lock    *dbconn.TableLockSession
	state   *runState
	applied appliedSource
	err     error
}

// loop saves the watermark every checkpoint interval until ctx ends. The
// context ending is how the copy's end is signalled, so it is not an
// error, whether it ends the sleep or a save in flight: the save after
// the copier returns records what that save did not.
func (s *watermarkSaver) loop(ctx context.Context, c *copier.Copier) error {
	interval := s.runner.opts.checkpointInterval()
	sleep := s.runner.opts.sleep()
	for {
		if err := sleep(ctx, interval); err != nil {
			return s.unlessEnded(ctx, err)
		}
		if err := s.saveIfMoved(ctx, c); err != nil {
			return s.unlessEnded(ctx, err)
		}
	}
}

// unlessEnded returns err unless ctx has ended, which is the copy's end
// and not a failure of the saver's own.
func (s *watermarkSaver) unlessEnded(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// saveIfMoved writes the row in PhaseCopying with the copier's landed
// watermark when it has moved past the one the row carries, and the
// applied position as far as it has moved.
func (s *watermarkSaver) saveIfMoved(ctx context.Context, c *copier.Copier) error {
	saved := s.state.cp
	applied := saved.LastAppliedLSN
	if s.applied != nil {
		applied = max(applied, s.applied())
	}
	landed := c.Position().Watermark
	if !landed.Valid() || landed == saved.Watermark {
		return nil
	}
	s.state.cp.Watermark = landed
	s.state.cp.LastAppliedLSN = applied
	if err := s.runner.save(ctx, s.lock, s.state, checkpoint.PhaseCopying); err != nil {
		// The row still carries the last save that succeeded, so the state
		// reads the same.
		s.state.cp = saved
		return err
	}
	return nil
}
