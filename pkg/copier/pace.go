package copier

import (
	"context"
	"fmt"
	"time"
)

const (
	// DefaultPacePoll is the wait between two readings of the backlog while
	// a worker is held back, when Pace leaves Poll zero.
	DefaultPacePoll = 100 * time.Millisecond
	// DefaultPaceMaxWait is the longest a worker is held back before one
	// chunk, when Pace leaves MaxWait zero.
	DefaultPaceMaxWait = time.Minute
)

// Pace holds the copy back while a backlog its caller measures stands over
// a mark, so the copy writes no faster than whatever follows it can absorb.
// The copier knows nothing of what the backlog is: a live run measures the
// WAL its change stream has yet to confirm, which the copy's own inserts
// add to. The zero Pace paces nothing.
//
// A worker reads the backlog before it claims a chunk, never while one is
// in flight, so a held-back copy has no chunk open: nothing it has claimed
// keeps the applier from writing, and the backlog can fall. The wait is a
// brake and not a gate — a worker held for MaxWait claims its chunk with
// the backlog still over the mark, so a backlog the copy is not the cause
// of slows the copy to one chunk per worker per MaxWait and never stops it;
// stopping the run is left to whoever owns the backlog's hard limit.
type Pace struct {
	// Backlog reads the backlog as it stands, in the caller's unit. It is
	// called from every worker, so it must be safe to call concurrently and
	// should not block. Nil means the copy is not paced and the other
	// fields are refused.
	Backlog func() uint64
	// HighWater is the backlog above which a worker waits before claiming
	// its next chunk; it claims once a reading is at or below it.
	HighWater uint64
	// Poll is the wait between two readings while a worker is held back.
	Poll time.Duration
	// MaxWait bounds how long a worker is held back before one chunk.
	MaxWait time.Duration
	// Sleep waits for d or until ctx ends, returning ctx's error then; nil
	// waits on the wall clock. Tests inject one to drive the wait without
	// real time passing.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Paced is how far a copier's Pace held its run back.
type Paced struct {
	// Waits is the number of chunk claims that waited, one per worker per
	// chunk it was held back before.
	Waits int64
	// Expired is how many of those waits ended at MaxWait with the backlog
	// still over the mark.
	Expired int64
	// Waited is the time the workers spent held back, summed over them, so
	// it can exceed the copy's elapsed time.
	Waited time.Duration
}

func (p Pace) withDefaults() Pace {
	if p.Backlog == nil {
		return p
	}
	if p.Poll == 0 {
		p.Poll = DefaultPacePoll
	}
	if p.MaxWait == 0 {
		p.MaxWait = DefaultPaceMaxWait
	}
	if p.Sleep == nil {
		p.Sleep = sleepContext
	}
	return p
}

// validate runs after withDefaults. A Pace with no Backlog must be the
// zero Pace, so a mark set without a reading to judge it by is refused
// instead of silently pacing nothing.
func (p Pace) validate() error {
	if p.Backlog == nil {
		if p.HighWater != 0 || p.Poll != 0 || p.MaxWait != 0 || p.Sleep != nil {
			return fmt.Errorf("%w: pace has no backlog to read; set Backlog or leave the pace zero", ErrInvalidOptions)
		}
		return nil
	}
	if p.HighWater == 0 {
		return fmt.Errorf("%w: pace high-water mark is zero, which would hold the copy back on any backlog at all", ErrInvalidOptions)
	}
	if p.Poll < 0 {
		return fmt.Errorf("%w: pace poll %s is negative; use zero for the default", ErrInvalidOptions, p.Poll)
	}
	if p.MaxWait < 0 {
		return fmt.Errorf("%w: pace max wait %s is negative; use zero for the default", ErrInvalidOptions, p.MaxWait)
	}
	return nil
}

// over reports whether the backlog as it reads now is above the mark.
func (p Pace) over() bool {
	return p.Backlog() > p.HighWater
}

// awaitPace holds the calling worker back while the backlog is over the
// mark, reading it every Poll, for MaxWait at the most. It returns nil when
// the worker may claim its chunk — at once when the copy is not paced or
// the backlog is at or below the mark — and ctx's error when ctx ended the
// wait.
func (c *Copier) awaitPace(ctx context.Context) error {
	pace := c.opts.Pace
	if pace.Backlog == nil || !pace.over() {
		return nil
	}
	c.paceWaits.Add(1)
	for waited := time.Duration(0); waited < pace.MaxWait; {
		step := min(pace.Poll, pace.MaxWait-waited)
		if err := pace.Sleep(ctx, step); err != nil {
			return err
		}
		waited += step
		c.paceWaited.Add(int64(step))
		if !pace.over() {
			return nil
		}
	}
	c.paceExpired.Add(1)
	return nil
}

// Paced is how far the copier's Pace has held this run back so far; the
// zero Paced for a copy that is not paced or never met a backlog over the
// mark. It is safe to read from any goroutine while Run is in progress.
func (c *Copier) Paced() Paced {
	return Paced{
		Waits:   c.paceWaits.Load(),
		Expired: c.paceExpired.Load(),
		Waited:  time.Duration(c.paceWaited.Load()),
	}
}

// sleepContext waits for d on the wall clock or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
