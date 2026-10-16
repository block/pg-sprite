package schemachange

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// drainWait is how long one read of the drain's stream waits for the next
// change before it reports the stream's position instead: the pace the
// drain learns that the stream has delivered everything through its
// marker.
const drainWait = 100 * time.Millisecond

// drain is the DrainFunc a live run hands the swap: inside the swap
// transaction, with ACCESS EXCLUSIVE on the source excluding every
// writer, it applies every change committed on the source since the
// position the row carries, so the renamed shadow holds every row the
// source held at the swap (CO-1, ST-4).
//
// Each attempt derives what to apply from durable state alone: it marks
// the WAL from a connection of its own, opens a stream on the row's slot
// at the row's position, gathers every change the stream delivers through
// the marker — every write that committed before the lock was granted
// lies below it — closes the stream, and flushes the gathered changes in
// tx. The stream is never confirmed, so a swap attempt that rolls back
// leaves the slot where it was and the next attempt gathers the same
// changes again (DrainFunc).
func (r *Runner) drain(pool *pgxpool.Pool, target preflight.CopySwapTarget, built BuiltShadow, flusher *applier.Flusher, watermark copier.Watermark, from decode.LSN) DrainFunc {
	return func(ctx context.Context, tx pgx.Tx) error {
		if err := r.drainInto(ctx, pool, tx, target, built, flusher, watermark, from); err != nil {
			return fmt.Errorf("drain %s.%s: %w", target.Schema(), target.Table(), err)
		}
		return nil
	}
}

func (r *Runner) drainInto(ctx context.Context, pool *pgxpool.Pool, tx pgx.Tx, target preflight.CopySwapTarget, built BuiltShadow, flusher *applier.Flusher, watermark copier.Watermark, from decode.LSN) error {
	marker, err := markWAL(ctx, pool)
	if err != nil {
		return err
	}
	buffer := applier.NewBuffer()
	if err := r.gatherThrough(ctx, pool, target, from, marker, buffer); err != nil {
		return err
	}
	if err := flushAll(ctx, tx, flusher, buffer, watermark); err != nil {
		return err
	}
	// The flush leaves the search_path on the catalog; the swap's own
	// statements resolve names under the target schema.
	return setSearchPath(ctx, tx, built.Schema())
}

// markWAL writes a marker to the WAL in a transaction of its own on pool
// and returns the position just past it. The marker's transaction commits
// on a connection outside the swap transaction, so its position lies
// above every write the lock excluded.
func markWAL(ctx context.Context, pool *pgxpool.Pool) (decode.LSN, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("mark the WAL position: %w", err)
	}
	marker, err := decode.MarkWAL(ctx, tx)
	if err != nil {
		return 0, errors.Join(err, rollback(ctx, tx))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("mark the WAL position: %w", err)
	}
	return marker, nil
}

// rollback rolls tx back after a failure, outliving the caller's context
// since a cancelled statement is one way here; a transaction the server
// has already ended reports nothing.
func rollback(ctx context.Context, tx pgx.Tx) error {
	err := tx.Rollback(context.WithoutCancel(ctx))
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return fmt.Errorf("roll back: %w", err)
}

// gatherThrough opens a stream on the target's slot at from, adds every
// change it delivers to buffer until the stream has delivered through
// marker, then closes the stream and waits for its backend to let the
// slot go. The gather is bounded by the swap's statement budget: a
// stream that cannot reach the marker within it fails the attempt, as a
// statement that ran that long would.
func (r *Runner) gatherThrough(ctx context.Context, pool *pgxpool.Pool, target preflight.CopySwapTarget, from, marker decode.LSN, buffer *applier.Buffer) error {
	readCtx, cancel := context.WithTimeout(ctx, r.opts.Cutover.statementTimeout())
	defer cancel()
	stream, err := decode.OpenStream(readCtx, r.cfg, pool, target, from)
	if err != nil {
		return err
	}
	gathered := gather(readCtx, stream, marker, buffer)
	endCtx, cancelEnd := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
	defer cancelEnd()
	if err := closeStream(endCtx, stream); err != nil {
		return errors.Join(gathered, err)
	}
	if gathered != nil {
		return gathered
	}
	return awaitSlotIdle(endCtx, pool, stream.Slot(), r.opts.sleep())
}

// gather reads stream into buffer until it has delivered through marker.
func gather(ctx context.Context, stream *decode.Stream, marker decode.LSN, buffer *applier.Buffer) error {
	for {
		delivery, err := stream.Next(ctx, drainWait)
		if err != nil {
			return fmt.Errorf("gather changes through %s: %w", marker, err)
		}
		if delivery.Change != nil {
			if err := buffer.Add(*delivery.Change); err != nil {
				return err
			}
		}
		if delivery.Delivered >= marker {
			return nil
		}
	}
}

// flushAll applies everything buffer holds into the shadow in tx. Every
// key is landed, so the whole buffer drains in one batch; images the
// flush completed but held back are released and flushed in a second
// batch, since under the swap's lock no later change can land on the row
// they were read from. A batch the shadow refuses cannot converge (CO-6):
// no copy is left to change the outcome. A buffer with anything left
// after the second flush, or a second flush that holds images again, is
// an invariant violation: the drain would rename a shadow short of a
// change it gathered.
func flushAll(ctx context.Context, tx pgx.Tx, flusher *applier.Flusher, buffer *applier.Buffer, watermark copier.Watermark) error {
	landed := copier.Position{Watermark: watermark, Cut: watermark}
	result, err := flushBatch(ctx, tx, flusher, buffer.Drain(landed))
	if err != nil {
		return err
	}
	if len(result.Held) > 0 {
		if err := buffer.Hold(result.Held); err != nil {
			return err
		}
		buffer.Release(decode.LSN(math.MaxUint64))
		again, err := flushBatch(ctx, tx, flusher, buffer.Drain(landed))
		if err != nil {
			return err
		}
		if len(again.Held) > 0 {
			// INV: CO-8
			return fmt.Errorf("%w (CO-8): the drain held %d images back from a shadow no writer can change", ErrInvariantViolation, len(again.Held))
		}
	}
	if buffer.Len() > 0 {
		// INV: CO-4
		return fmt.Errorf("%w (CO-4): the drain left %d changes unapplied under the swap's lock", ErrInvariantViolation, buffer.Len())
	}
	return nil
}

// flushBatch flushes one batch in tx and reads a refusal as the
// non-convergence it is.
func flushBatch(ctx context.Context, tx pgx.Tx, flusher *applier.Flusher, batch applier.Batch) (applier.Result, error) {
	result, err := flusher.FlushIn(ctx, tx, batch)
	if errors.Is(err, applier.ErrBatchDeferred) {
		// INV: CO-6
		return applier.Result{}, refuse(CauseDrainDeferred, []error{err}, "the shadow refused %d changes the swap had to apply under its lock", len(batch.Entries))
	}
	return result, err
}
