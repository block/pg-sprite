package schemachange

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
