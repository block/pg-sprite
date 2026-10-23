package schemachange

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

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
//
// The drain draws nothing from the pool: tx holds a pool connection for
// the whole swap, and a drain that waited on the pool for a second one
// would wait on every other run's swap doing the same, so as many swaps
// at once as the pool has connections would each wait on the others. The
// marker, the stream's proof of its server, and the wait for the slot to
// go idle all run on one connection dialled from cfg for the attempt and
// closed with it.
func (r *Runner) drain(target preflight.CopySwapTarget, built BuiltShadow, flusher *applier.Flusher, watermark copier.Watermark, from decode.LSN) DrainFunc {
	return func(ctx context.Context, tx pgx.Tx) error {
		if err := r.drainInto(ctx, tx, target, built, flusher, watermark, from); err != nil {
			return fmt.Errorf("drain %s.%s: %w", target.Schema(), target.Table(), err)
		}
		return nil
	}
}

func (r *Runner) drainInto(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, built BuiltShadow, flusher *applier.Flusher, watermark copier.Watermark, from decode.LSN) (err error) {
	conn, err := dbconn.ConnectSession(ctx, r.cfg)
	if err != nil {
		return fmt.Errorf("connect for the drain: %w", err)
	}
	defer func() {
		err = errors.Join(err, closeDrainSession(ctx, conn))
	}()
	marker, err := markWAL(ctx, conn)
	if err != nil {
		return err
	}
	buffer := applier.NewBuffer()
	if err := r.gatherThrough(ctx, conn, target, from, marker, buffer); err != nil {
		return err
	}
	if err := flushAll(ctx, tx, flusher, buffer, watermark); err != nil {
		return err
	}
	// The flush leaves the search_path on the catalog; the swap's own
	// statements resolve names under the target schema.
	return setSearchPath(ctx, tx, built.Schema())
}

// txBeginner opens a transaction: a pool, or one connection of the
// caller's.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// markWAL writes a marker to the WAL in a transaction of its own on db and
// returns the position just past it. The marker's transaction commits on
// a connection outside the swap transaction, so its position lies above
// every write the lock excluded.
func markWAL(ctx context.Context, db txBeginner) (decode.LSN, error) {
	tx, err := db.Begin(ctx)
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
// slot go, reading the slot on conn. The gather is bounded by the swap's
// statement budget: a stream that cannot reach the marker within it fails
// the attempt, as a statement that ran that long would.
func (r *Runner) gatherThrough(ctx context.Context, conn *pgx.Conn, target preflight.CopySwapTarget, from, marker decode.LSN, buffer *applier.Buffer) error {
	readCtx, cancel := context.WithTimeout(ctx, r.opts.Cutover.statementTimeout())
	defer cancel()
	stream, err := decode.OpenStream(readCtx, r.cfg, conn, target, from)
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
	return awaitSlotIdle(endCtx, conn, stream.Slot(), r.opts.sleep())
}

// closeDrainSession closes the drain's connection, even when ctx has
// ended: a cancelled swap attempt ends its drain too.
func closeDrainSession(ctx context.Context, conn *pgx.Conn) error {
	if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("close the drain's connection: %w", err)
	}
	return nil
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
	// Every writer of the source finished, and its commit became visible,
	// before the swap's lock was granted, so no gathered change can be
	// invisible here.
	visible := applier.AllVisible()
	result, err := flushBatch(ctx, tx, flusher, buffer.Drain(landed, visible))
	if err != nil {
		return err
	}
	if len(result.Held) > 0 {
		if err := buffer.Hold(result.Held); err != nil {
			return err
		}
		buffer.Release(decode.LSN(math.MaxUint64))
		again, err := flushBatch(ctx, tx, flusher, buffer.Drain(landed, visible))
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
