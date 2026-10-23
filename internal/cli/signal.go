package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// exitCodeInterrupted is the conventional shell exit status for a process
// ended by SIGINT (128 + the signal number).
const exitCodeInterrupted = 128 + int(syscall.SIGINT)

// errInterrupted is the cancellation cause recorded when an operator's
// signal stops a run. It wraps context.Canceled, so a caller that reads
// the cause (context.Cause) sees the same ordinary cancellation as one
// that reads ctx.Err(), and the run's verdict is cancelled-by-caller
// whichever phase the signal lands in.
var errInterrupted = fmt.Errorf("interrupted by signal: %w", context.Canceled)

// Interruptible derives a context that an operator's SIGINT or SIGTERM
// cancels, and returns the stop function that releases the signal
// handler. The first signal cancels the context so the running command
// unwinds through its own stop path: a copy-and-swap run releases its
// table lock, writes its checkpoint, and prints the cancelled verdict, so a
// later run of the same statement resumes. A second signal ends the process
// immediately with exitCodeInterrupted for an operator who cannot wait.
func Interruptible(ctx context.Context) (context.Context, func()) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	ctx, stop := twoStage(ctx, sigs, os.Exit)
	return ctx, func() {
		signal.Stop(sigs)
		stop()
	}
}

// twoStage is Interruptible's signal policy with the signal source and the
// process exit injected: the first signal cancels the context with
// errInterrupted as its cause, the second calls exit. The goroutine ends
// when stop is called or the parent context ends, whichever comes first;
// stop also cancels the derived context, since the command it guarded has
// returned by then.
func twoStage(ctx context.Context, sigs <-chan os.Signal, exit func(int)) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			cancel(nil)
		})
	}
	go func() {
		select {
		case <-sigs:
			cancel(errInterrupted)
		case <-done:
			return
		case <-ctx.Done():
			return
		}
		select {
		case <-sigs:
			exit(exitCodeInterrupted)
		case <-done:
		}
	}()
	return ctx, stop
}
