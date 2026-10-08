package applier

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/decode"
)

// HeldImage is a moved image a Flush completed but did not write. The value
// its markers stand for was read from a row that can be newer than the
// stream — the old key's shadow row, which the copier read live, or the
// source row under Key — so a change the stream has not delivered yet could
// have put another row's value there. The flush hands the completion back
// with the WAL position of its read, and the buffer writes the image in a
// later flush once the stream has delivered everything the read could have
// seen and nothing touched the key (D13).
type HeldImage struct {
	// Entry is the image as the batch carried it, markers intact.
	Entry Entry
	// Completed carries the value the read found for each marker column.
	Completed []decode.Column
	// FromSource reports a read of the source row under Key; false is a read
	// of the shadow row under the old key.
	FromSource bool
	// ReadLSN is the WAL insert position at the read. Every change the read
	// saw had committed below it, so a stream that has delivered every
	// change committed through ReadLSN has delivered every change the read
	// knew about.
	ReadLSN decode.LSN
}

// pendingCompletion is the completion a held image waits to take.
type pendingCompletion struct {
	completed []decode.Column
	readLSN   decode.LSN
}

// Hold puts the images a Flush held back into the buffer, each as the batch
// carried it, with its completion pending: Drain keeps the image until
// Release lets the completion in, and an event that replaces the key's
// entry or reuses its old key drops the completion instead, since the read
// may have seen that event's row. Like Requeue, Hold runs before any Add
// after the Drain; a key the buffer already holds is ErrInvariantViolation,
// and the buffer is left as it was.
func (b *Buffer) Hold(held []HeldImage) error {
	// INV: CO-5, CO-8
	for _, h := range held {
		if _, taken := b.entries[h.Entry.Key]; taken {
			return fmt.Errorf("%w (CO-5): hold key %d: the buffer already holds an entry for it", ErrInvariantViolation, h.Entry.Key)
		}
	}
	for _, h := range held {
		e := h.Entry
		e.Columns = cloneColumns(e.Columns)
		b.put(&e)
		b.pending[e.Key] = pendingCompletion{completed: cloneColumns(h.Completed), readLSN: h.ReadLSN}
	}
	return nil
}

// Release tells the buffer that the stream has delivered every change
// committed at or below passed. A completion read at or below that position
// whose key no event has replaced since was read from the row the stream
// knows under the key, so the buffer takes it for every column the image
// still carries as a marker — an UPDATE of the key that arrived meanwhile
// kept the completion pending, and its values are newer than the read's —
// and the next Drain flushes the whole image. Release returns how many
// completions it took.
func (b *Buffer) Release(passed decode.LSN) int {
	// INV: CO-8
	taken := 0
	for key, p := range b.pending {
		if p.readLSN > passed {
			continue
		}
		b.entries[key].fill(p.completed)
		delete(b.pending, key)
		taken++
	}
	return taken
}

// Held reports how many images are buffered with a completion pending.
func (b *Buffer) Held() int { return len(b.pending) }

// dropPending forgets the pending completion under key, if any.
func (b *Buffer) dropPending(key int64) {
	delete(b.pending, key)
}
