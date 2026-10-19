package applier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Pause whose context has already ended still takes a gate that is free:
// the hold it asks for is there to be had, and refusing it would fail a
// caller whose context ended just as the last flush returned. The gate is
// taken (a cycle's flush finds it held) and the resume gives it back. With
// both the gate and the ended context ready, only the free-gate check ahead
// of the wait decides the outcome, so the Pause is asked for many times to
// leave a random choice between the two no room to pass.
func TestCatchupPauseTakesAFreeGateOnAnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := &Catchup{flushes: newFlushGate(), flusher: &Flusher{}}
	const attempts = 200
	for range attempts {
		resume, err := c.Pause(ctx)
		require.NoError(t, err, "a free gate is taken on an ended context")
		assert.False(t, c.tryFlushes(), "the Pause holds the gate, so a cycle's flush is skipped")
		assert.True(t, c.Status().FlushesHeld)
		resume()
		assert.False(t, c.Status().FlushesHeld)
		require.True(t, c.tryFlushes(), "the resume gave the gate back")
		c.releaseFlushes()
	}
}
