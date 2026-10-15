package schemachange

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// copy runs the copier from the row's watermark to the end of the key
// space, saving the landed watermark every checkpoint interval while the
// copy is in progress and once more when it ends, and leaves the row in
// PhaseVerifying with the complete watermark (TM-3).
//
// The periodic save runs beside the copier and stops it when a save
// fails: a run whose row cannot be written has nothing to resume from
// past the last save, so it stops rather than copy on unrecorded. The
// saved watermark only ever reads what has landed, so a row written
// mid-copy and resumed re-copies at most the chunks that were in flight.
func (r *Runner) copy(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, state *runState) error {
	c, err := copier.NewCopier(target, state.built, lock, state.cp.Watermark, r.opts.Copier)
	if err != nil {
		return copyRefusal(err, state.cp.Schema, state.cp.Table)
	}
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	saver := &watermarkSaver{runner: r, lock: lock, state: state}
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
		return saver.err
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
	return r.save(ctx, lock, state, checkpoint.PhaseVerifying)
}

// watermarkSaver writes the copier's landed watermark to the checkpoint
// row while the copy is in progress. It is used from the saving goroutine
// alone until that goroutine has been waited for, then from the caller.
type watermarkSaver struct {
	runner *Runner
	lock   *dbconn.TableLockSession
	state  *runState
	err    error
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
// watermark when it has moved past the one the row carries.
func (s *watermarkSaver) saveIfMoved(ctx context.Context, c *copier.Copier) error {
	landed := c.Position().Watermark
	saved := s.state.cp.Watermark
	if !landed.Valid() || landed == saved {
		return nil
	}
	s.state.cp.Watermark = landed
	if err := s.runner.save(ctx, s.lock, s.state, checkpoint.PhaseCopying); err != nil {
		// The row still carries the watermark of the last save that
		// succeeded, so the state reads the same.
		s.state.cp.Watermark = saved
		return err
	}
	return nil
}
