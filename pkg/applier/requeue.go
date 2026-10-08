package applier

import "fmt"

// Requeue puts a drained batch back into the buffer after a Flush returned
// ErrBatchDeferred, so the next Drain judges its entries again once the
// copier has moved. The entries go back as the drain handed them out: the
// flush wrote nothing and completed nothing the batch keeps, so a marker is
// still a marker. The stream owner adds nothing between the Drain and the
// Requeue, so no buffered entry can hold a batch key; one that does is
// ErrInvariantViolation, and the buffer is left as it was.
func (b *Buffer) Requeue(batch Batch) error {
	// INV: CO-5
	for _, e := range batch.Entries {
		if _, held := b.entries[e.Key]; held {
			return fmt.Errorf("%w (CO-5): requeue key %d: the buffer already holds an entry for it", ErrInvariantViolation, e.Key)
		}
	}
	for i := range batch.Entries {
		e := batch.Entries[i]
		b.put(&e)
	}
	return nil
}
