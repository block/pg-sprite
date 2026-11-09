package schemachange

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
)

// CatchupWaitError is the error a live run returns when the wait for the
// capture to confirm past a marker, written to the WAL at the start of the
// wait, ended before it did: the run had not caught up when the caller
// stopped it. Status is the catch-up's status when the wait ended, so the
// error says what the wait was for — a change whose transaction the source
// had not made visible (a commit waiting on a synchronous standby or a
// flush), one deferred behind an in-flight chunk or a refused batch, one
// held for a read the flush made, or plainly a stream behind the marker —
// where the bare context error could not tell a stalled source from a slow
// copy. Err is what ended the wait, and Unwrap exposes it, so the error
// still reads as the cancellation it is (errors.Is against
// context.Canceled or context.DeadlineExceeded). The row keeps the phase
// it waited in, catching up or, for a row resumed there, verifying, for
// the next Run to resume.
type CatchupWaitError struct {
	// Schema and Table name the target whose catch-up was waited for.
	Schema, Table string
	// Marker is the WAL position the capture had to confirm past.
	Marker decode.LSN
	// Status is the catch-up's status when the wait ended.
	Status applier.Status
	// Err is what ended the wait: the context's error, or whatever an
	// injected RunOptions.Sleep returned.
	Err error
}

// ShortOfMarker is how many WAL bytes the confirmed position trailed the
// marker when the wait ended. Unlike Status.Lag, which needs a completed
// cycle to measure the server's write position, it is known from the
// first moment of the wait, and it is the same quantity on every retry.
func (e *CatchupWaitError) ShortOfMarker() uint64 {
	if e.Status.Confirmed >= e.Marker {
		return 0
	}
	return uint64(e.Marker - e.Status.Confirmed)
}

func (e *CatchupWaitError) Error() string {
	return fmt.Sprintf("catch up %s.%s to %s: %v; the capture had confirmed %s, %d bytes short of the marker; changes waiting: %d not yet visible on the source, %d deferred behind an in-flight chunk or a refused batch, %d held for a read the flush made",
		e.Schema, e.Table, e.Marker, e.Err, e.Status.Confirmed, e.ShortOfMarker(), e.Status.Invisible, e.Status.Deferred, e.Status.Held)
}

func (e *CatchupWaitError) Unwrap() error { return e.Err }
