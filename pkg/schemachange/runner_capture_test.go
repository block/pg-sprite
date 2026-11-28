package schemachange

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
)

// A fill cancelled by a capture that failed stopped on the capture's
// doing: the run reports the capture's failure alone, not a cancellation
// of the fill's own. That holds while the run's context is live. When the
// run's own context ended, the capture and the fill stopped on the same
// stop, and what the fill was doing when it stopped — here the marker wait
// and what the capture was waiting on — is kept alongside the stop the
// capture reports.
func TestCaptureOutcomeKeepsTheFillsErrorWhenTheRunsContextEnded(t *testing.T) {
	ownerStop := errors.New("owner is ready to cut over")
	wait := &CatchupWaitError{Schema: "public", Table: "orders", Marker: 100, Err: context.Canceled}

	t.Run("capture failed under a live run", func(t *testing.T) {
		c := &capture{err: fmt.Errorf("stream: %w", decode.ErrSlotLost)}
		fillErr := fmt.Errorf("copy chunk: %w", context.Canceled)
		err := captureOutcome(t.Context(), c, fillErr, nil)
		require.ErrorIs(t, err, decode.ErrSlotLost)
		assert.NotErrorIs(t, err, context.Canceled, "the fill's cancellation was the capture's doing, not a failure of its own")
	})

	t.Run("run's context ended during the marker wait", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(ownerStop)
		c := &capture{err: fmt.Errorf("catch up: %w", context.Cause(ctx))}
		err := captureOutcome(ctx, c, wait, nil)
		var found *CatchupWaitError
		require.ErrorAs(t, err, &found, "what the fill was waiting on is kept")
		assert.Equal(t, decode.LSN(100), found.Marker)
		require.ErrorIs(t, err, ownerStop, "and so is the owner's cause the capture stopped on")
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("capture ended cleanly by the run", func(t *testing.T) {
		c := &capture{err: fmt.Errorf("catch up: %w", errCaptureEnded)}
		err := captureOutcome(t.Context(), c, nil, nil)
		require.NoError(t, err, "a capture its owner stopped reports nothing")
	})
}

// A capture's end bounds its own wait for the slot. When that bound runs
// out, the error it returns names the expiry and does not match
// context.DeadlineExceeded, since the caller's context did not end: a
// slot lag stop joined with it keeps its own code rather than reading as
// the caller's cancellation. An error returned while the bound is still
// live comes back unchanged.
func TestCaptureEndWaitExpiryIsNotAContextError(t *testing.T) {
	waiting := fmt.Errorf("await slot pgsprite_x idle, held by backend 4242: %w", context.DeadlineExceeded)

	t.Run("the end's own bound expired", func(t *testing.T) {
		endCtx, cancel := context.WithTimeoutCause(context.WithoutCancel(t.Context()), 0, errCaptureEndWaitExpired)
		defer cancel()
		<-endCtx.Done()
		err := endWaitError(endCtx, waiting)
		require.ErrorIs(t, err, errCaptureEndWaitExpired)
		assert.NotErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, context.Canceled)
	})

	t.Run("the bound is still live", func(t *testing.T) {
		endCtx, cancel := context.WithTimeoutCause(context.WithoutCancel(t.Context()), time.Hour, errCaptureEndWaitExpired)
		defer cancel()
		err := endWaitError(endCtx, waiting)
		assert.Same(t, waiting, err)
	})
}

// A catch-up that failed while its owner was stopping it returns the stop
// with the cycle's failure wrapped after it. Every failure the capture
// can raise is read as the capture's own through that wrapping, so the run
// reports it instead of a clean stop; the stop alone is not a failure.
func TestCaptureFailedReadsEveryFailureThroughTheStop(t *testing.T) {
	failures := map[string]error{
		"invariant violation":   applier.ErrInvariantViolation,
		"table lock lost":       applier.ErrTableLockLost,
		"batch deferred":        applier.ErrBatchDeferred,
		"batch deadlocked":      applier.ErrBatchDeadlocked,
		"stream ended":          decode.ErrStreamEnded,
		"slot lost":             decode.ErrSlotLost,
		"slot lag ceiling":      decode.ErrSlotLagCeiling,
		"slot lag unknown":      fmt.Errorf("%w: slot pg_sprite_orders", decode.ErrSlotLagUnknown),
		"unsupported change":    decode.ErrUnsupportedChange,
		"source shape changed":  decode.ErrSourceShapeChanged,
		"slot lost, typed":      &decode.SlotLostError{Slot: "pg_sprite_orders"},
		"slot lag over ceiling": &decode.SlotLagExceededError{Slot: "pg_sprite_orders", Retained: 2, Ceiling: 1},
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			cycle := fmt.Errorf("catch-up on public.orders: %w", failure)
			err := fmt.Errorf("catch-up on public.orders stopped: %w (%w); the cycle it stopped in failed: %w", errCaptureEnded, context.Canceled, cycle)
			assert.True(t, captureFailed(err))
			assert.Same(t, err, (&capture{err: err}).failure(), "the stop that carried a failure is the failure")
		})
	}

	stop := fmt.Errorf("catch-up on public.orders stopped: %w (%w)", errCaptureEnded, context.Canceled)
	assert.False(t, captureFailed(stop), "the owner's stop alone")
	assert.NoError(t, (&capture{err: stop}).failure())
	assert.False(t, captureFailed(context.Canceled), "a plain cancellation")
}
