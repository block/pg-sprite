package applier

import (
	"context"
	"fmt"
	"sync"
)

// Pause holds the catch-up's flushes off the shadow until resume is called.
// It returns once no flush is in flight, so a caller that then takes the
// shadow for itself — a live repair recopying it from the source — never
// waits behind a flush, and the stream never waits behind the caller. The
// catch-up keeps running under the hold: every cycle still reads the
// stream, so the walsender's keepalives are answered and nothing it sends
// is lost, buffers what arrives, and confirms what it may — with unapplied
// changes buffered that is the position below the oldest of them (ST-4) —
// and only the drain and the flush are skipped, so the buffer grows by
// what the source writes for as long as the hold lasts and the first cycle
// after resume flushes it. The hold is bounded by the caller: a caller
// that waits for the catch-up to confirm past its own work resumes first,
// since the confirmation needs the flushes. Pause waits for a flush in
// flight to commit or fail, or for ctx to end; a second Pause waits for the
// first's resume; resume is safe to call more than once. Pause before Run
// takes the hold at once, and a Run started under it flushes nothing until
// resume. Status reports the hold as FlushesHeld from Pause's return until
// the resume.
func (c *Catchup) Pause(ctx context.Context) (resume func(), err error) {
	if err := c.holdFlushes(ctx); err != nil {
		return nil, err
	}
	c.markFlushesHeld(true)
	var once sync.Once
	return func() {
		once.Do(func() {
			c.markFlushesHeld(false)
			c.releaseFlushes()
		})
	}, nil
}

// markFlushesHeld records in the status whether a Pause holds the flushes.
// Only Pause and its resume call it: a cycle's own flush takes the gate
// too, but is not a hold.
func (c *Catchup) markFlushesHeld(held bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.FlushesHeld = held
}

// holdFlushes takes the flush gate, waiting for a cycle's flush to give it
// back. A gate that is free is taken even on an ended ctx, so a caller
// whose context ends as the flush returns is not refused a hold it could
// have had.
func (c *Catchup) holdFlushes(ctx context.Context) error {
	select {
	case <-c.flushes:
		return nil
	default:
	}
	select {
	case <-c.flushes:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pause the catch-up on %s.%s: %w", c.flusher.target.Schema(), c.flusher.target.Table(), context.Cause(ctx))
	}
}

// releaseFlushes gives the flush gate back.
func (c *Catchup) releaseFlushes() {
	c.flushes <- struct{}{}
}

// tryFlushes takes the flush gate for one cycle's flush when nothing holds
// it, and reports the flushes paused when a Pause does. A cycle never
// waits for the gate: it has the stream to keep reading.
func (c *Catchup) tryFlushes() bool {
	select {
	case <-c.flushes:
		return true
	default:
		return false
	}
}

// newFlushGate is the gate of a catch-up nothing has paused: one token,
// held by whoever is flushing or pausing and free otherwise.
func newFlushGate() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}
