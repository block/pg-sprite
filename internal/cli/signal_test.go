package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signalTestDeadline bounds every wait in these tests; the goroutine under
// test reacts to a channel send, so a wait this long means it is stuck.
const signalTestDeadline = 5 * time.Second

func TestFirstSignalCancelsTheContextAndKeepsTheProcessAlive(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	ctx, stop := twoStage(t.Context(), sigs, func(code int) { exited <- code })
	defer stop()

	sigs <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(signalTestDeadline):
		t.Fatal("the first signal did not cancel the context")
	}
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "callers branch on an ordinary cancellation")
	assert.ErrorIs(t, context.Cause(ctx), errInterrupted)
	select {
	case code := <-exited:
		t.Fatalf("the first signal ended the process with %d; it must only cancel", code)
	default:
	}
}

func TestSecondSignalEndsTheProcessWithTheInterruptedExitCode(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	ctx, stop := twoStage(t.Context(), sigs, func(code int) { exited <- code })
	defer stop()

	sigs <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(signalTestDeadline):
		t.Fatal("the first signal did not cancel the context")
	}
	sigs <- os.Interrupt
	select {
	case code := <-exited:
		assert.Equal(t, 130, code)
	case <-time.After(signalTestDeadline):
		t.Fatal("the second signal did not end the process")
	}
}

func TestStopReleasesTheDerivedContextWithoutTouchingTheParent(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	ctx, stop := twoStage(t.Context(), sigs, func(int) {})

	stop()
	select {
	case <-ctx.Done():
	case <-time.After(signalTestDeadline):
		t.Fatal("stop did not release the derived context")
	}
	require.NoError(t, t.Context().Err(), "the parent context is untouched")
}
