package testutil

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
)

// A pin whose clock is never read — its test ended before the copier
// reached a first chunk — ends at cleanup at once, with nothing to roll
// back, instead of waiting on a reading that will not come. The cleanup
// disarms the clock, so a reading after it places nothing: the pin has no
// pool, and any attempt to place it would fail.
func TestShadowPinCleanupReturnsWhenTheClockIsNeverRead(t *testing.T) {
	pin := newShadowPin(t.Context(), nil, pgx.Identifier{"never_read", "shadow"}, 1, 0)

	ended := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { ended <- pin.end(context.WithoutCancel(t.Context())) })
	const cleanupDeadline = 5 * time.Second
	select {
	case err := <-ended:
		assert.NoError(t, err, "a pin that was never placed has nothing to report")
	case <-time.After(cleanupDeadline):
		// Disarm the clock the cleanup is stuck on so its goroutine ends.
		pin.clock.once.Do(func() { close(pin.pinned) })
		t.Errorf("the cleanup of a pin whose clock was never read did not return within %s", cleanupDeadline)
	}
	wg.Wait()

	assert.Nil(t, pin.tx, "no pin was placed")
	pin.Clock().Now()
	assert.Nil(t, pin.tx, "a reading after the cleanup places nothing")
}
