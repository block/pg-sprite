package applier

import (
	"context"

	"github.com/block/pg-sprite/pkg/progress"
)

// Work reports the catch-up's counters for the progress tracker: it is the
// progress.WorkSource the catch-up registers for the lifetime of Run. Every
// counter is read from memory — the catch-up knows what it has flushed and
// what the stream has told it — so a poll never waits on the database.
//
//   - changes_applied is the number of images and delete markers committed
//     flushes have written to the shadow, exact and monotone.
//   - changes_buffered is the number of keys the buffer held at the end of
//     the last cycle: changes waiting for an in-flight chunk or for a
//     completion the stream has not yet passed.
//   - lag_bytes is how far the confirmed position trails the server's
//     write position, in WAL bytes, both as of the last cycle: the write
//     position is pg_current_wal_lsn() read on the pool as the cycle
//     ended, never the walsender's send position, which stays small
//     while a backlog is undecoded. Zero until a cycle has run.
func (c *Catchup) Work(context.Context) (progress.Work, error) {
	s := c.Status()
	return progress.Work{
		ChangesApplied:  s.Applied,
		ChangesBuffered: uint64(s.Buffered),
		LagBytes:        s.Lag(),
	}, nil
}

// report registers the catch-up with the tracker, when there is one, for
// the rest of Run. The returned stop is the fence before Run returns: it
// waits for an in-flight poll, so no poll that began while Run ran
// completes against a catch-up whose caller has moved on.
func (c *Catchup) report() (stop func()) {
	if c.opts.Tracker != nil {
		c.opts.Tracker.SetWorkSource(c)
	}
	return func() {
		if c.opts.Tracker != nil {
			c.opts.Tracker.StopWorkSource()
		}
	}
}
