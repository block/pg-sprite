package schemachange

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
)

// CatchupWaitError is the error a live run returns when its context ended
// while it waited for the capture to confirm past a marker written to the
// WAL at the start of the wait: the run had not caught up when the caller
// stopped it. Status is the catch-up's status when the wait ended, so the
// error says what the wait was for — a change whose transaction the source
// had not made visible (a commit waiting on a synchronous standby or a
// flush), one deferred behind an in-flight chunk or a refused batch, one
// held for a read the flush made, or plainly a stream behind the server
// — where the bare context error could not tell a stalled source from a
// slow copy. Err is the context's error, and Unwrap exposes it, so the
// error still reads as the cancellation it is (errors.Is against
// context.Canceled or context.DeadlineExceeded) and the row stays
// catching up for the next Run to resume.
type CatchupWaitError struct {
	// Schema and Table name the target whose catch-up was waited for.
	Schema, Table string
	// Marker is the WAL position the capture had to confirm past.
	Marker decode.LSN
	// Status is the catch-up's status when the wait ended.
	Status applier.Status
	// Err is why the wait ended: the context's error.
	Err error
}

func (e *CatchupWaitError) Error() string {
	return fmt.Sprintf("catch up %s.%s to %s: %v; when the wait ended the capture had confirmed %s, %d bytes behind the server's write position, with %d changes waiting for the source to show their transactions visible, %d deferred behind an in-flight chunk or a refused batch, and %d held for a read the flush made",
		e.Schema, e.Table, e.Marker, e.Err, e.Status.Confirmed, e.Status.Lag(), e.Status.Invisible, e.Status.Deferred, e.Status.Held)
}

func (e *CatchupWaitError) Unwrap() error { return e.Err }
