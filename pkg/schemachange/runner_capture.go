package schemachange

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// errCaptureEnded is the cause a capture's context ends with when its
// owner stops it: the one end of a catch-up that is not a failure.
var errCaptureEnded = errors.New("capture ended by the run")

// slotIdlePoll is how often a run that has stopped its stream reads the
// slot while it waits for the stream's backend to let the slot go.
const slotIdlePoll = 50 * time.Millisecond

// capture is a live run's change capture: the stream on the row's slot and
// the catch-up applying it to the shadow, running on their own goroutine
// from startCapture until end. The run reads the catch-up's confirmed
// position while the capture runs; nothing else of it is shared.
type capture struct {
	stream  *decode.Stream
	catchup *applier.Catchup
	cancel  context.CancelCauseFunc
	wg      sync.WaitGroup
	// err is what the catch-up's Run returned; it is read after wg has
	// been waited for, or through failure.
	err error
}

// startCapture opens the stream at from and starts the catch-up applying
// it through flusher, reading positions for the keys it may write (CO-4).
// It returns the capture and the context the rest of the fill runs under:
// a catch-up that fails cancels it with its error as the cause, so the
// copy or the pass in progress stops instead of filling a shadow the
// stream has stopped keeping current.
func (r *Runner) startCapture(ctx context.Context, pool *pgxpool.Pool, target preflight.CopySwapTarget, flusher *applier.Flusher, positions applier.PositionSource, from decode.LSN) (*capture, context.Context, error) {
	stream, err := decode.OpenStream(ctx, r.cfg, pool, target, from)
	if err != nil {
		return nil, nil, err
	}
	catchup, err := applier.NewCatchup(stream, flusher, positions, r.opts.Catchup)
	if err != nil {
		return nil, nil, errors.Join(err, closeStream(ctx, stream))
	}
	liveCtx, cancel := context.WithCancelCause(ctx)
	c := &capture{stream: stream, catchup: catchup, cancel: cancel}
	c.wg.Go(func() {
		c.err = catchup.Run(liveCtx, pool)
		cancel(c.err)
	})
	return c, liveCtx, nil
}

// end stops the capture and waits for its goroutine, closes the stream,
// and waits for the slot to be let go by the stream's backend, so the
// next stream on the slot — the swap's drain — opens on a slot nobody
// holds. The stop and the wait outlive the caller's context, since a
// cancelled run has to end its capture too, but are bounded so they
// return against a server that has stopped answering.
func (c *capture) end(ctx context.Context, pool *pgxpool.Pool, sleep func(context.Context, time.Duration) error) error {
	c.cancel(errCaptureEnded)
	c.wg.Wait()
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
	defer cancel()
	if err := closeStream(endCtx, c.stream); err != nil {
		return err
	}
	return awaitSlotIdle(endCtx, pool, c.stream.Slot(), sleep)
}

// failure is the catch-up's error when it is the catch-up's own: anything
// but the clean stop its owner asked for. A stop that carried a failure
// sentinel out of the cycle it interrupted is the failure, not the stop.
func (c *capture) failure() error {
	if errors.Is(c.err, errCaptureEnded) && !captureFailed(c.err) {
		return nil
	}
	return c.err
}

// captureFailed reports whether a catch-up's error carries a failure of
// the capture's own, as distinct from the stop that ended it.
func captureFailed(err error) bool {
	sentinels := []error{
		applier.ErrInvariantViolation,
		applier.ErrTableLockLost,
		applier.ErrBatchDeferred,
		decode.ErrStreamEnded,
		decode.ErrSlotLost,
		decode.ErrSlotLagCeiling,
		decode.ErrUnsupportedChange,
		decode.ErrSourceShapeChanged,
	}
	for _, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	var lost *decode.SlotLostError
	var lag *decode.SlotLagExceededError
	return errors.As(err, &lost) || errors.As(err, &lag)
}

// captureOutcome folds what the fill and the capture's end returned into
// one error, with the catch-up's failure first when it has one: the fill
// then stopped on the capture's cancellation, which is not a failure of
// its own and is not reported as one.
func captureOutcome(c *capture, fillErr, endErr error) error {
	failure := c.failure()
	if failure != nil && errors.Is(fillErr, context.Canceled) {
		fillErr = nil
	}
	return errors.Join(failure, fillErr, endErr)
}

// closeStream closes a stream's replication connection.
func closeStream(ctx context.Context, stream *decode.Stream) error {
	if err := stream.Close(ctx); err != nil {
		return fmt.Errorf("close stream on slot %s: %w", stream.Slot(), err)
	}
	return nil
}

// awaitSlotIdle waits until no backend holds the slot, or ctx ends. The
// backend that served a closed stream leaves on its own once it notices
// the connection gone; until it has, the slot is active and no stream can
// open on it.
func awaitSlotIdle(ctx context.Context, pool *pgxpool.Pool, name string, sleep func(context.Context, time.Duration) error) error {
	for {
		status, found, err := decode.InspectSlot(ctx, pool, name)
		if err != nil {
			return fmt.Errorf("await slot %s idle: %w", name, err)
		}
		if !found || !status.Active {
			return nil
		}
		if err := sleep(ctx, slotIdlePoll); err != nil {
			return fmt.Errorf("await slot %s idle, held by backend %d: %w", name, status.ActivePID, err)
		}
	}
}
