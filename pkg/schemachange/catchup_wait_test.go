package schemachange_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A marker wait that the context ended still reads as that context's
// error — a caller that stopped the run is told it stopped, not that the
// run failed — while the typed error is reachable through the wrapping a
// run adds, and names the target and carries the catch-up's status so the
// caller reads what the wait was for.
func TestCatchupWaitErrorReadsAsTheContextErrorItWraps(t *testing.T) {
	wait := &schemachange.CatchupWaitError{
		Schema: "public", Table: "orders", Marker: 0x16B3748,
		Status: applier.Status{Confirmed: 0x16B3700, WALEnd: 0x16B3800, Buffered: 3, Invisible: 1, Deferred: 2},
		Err:    context.DeadlineExceeded,
	}
	err := fmt.Errorf("run public.orders: %w", wait)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, context.Canceled)
	var found *schemachange.CatchupWaitError
	require.ErrorAs(t, err, &found)
	assert.Equal(t, 1, found.Status.Invisible)
	assert.Equal(t, uint64(0x100), found.Status.Lag(), "the status measures how far the confirmed position trailed the server")

	text := wait.Error()
	assert.Contains(t, text, "public.orders", "the rendering names the target")
	assert.Contains(t, text, context.DeadlineExceeded.Error(), "and why the wait ended")
}
