package testutil

import (
	"sync"
	"time"
)

// HookedClock is a clock whose first reading runs a hook. The copier and
// the verifier read their clock once per chunk, after the chunk is claimed
// and before its transaction begins, so a hook there lands after the run's
// setup has committed and meets the first chunk as a concurrent writer.
type HookedClock struct {
	once sync.Once
	hook func()
}

// NewHookedClock returns a clock that runs hook on its first reading.
func NewHookedClock(hook func()) *HookedClock {
	return &HookedClock{hook: hook}
}

// Now runs the hook on the first call, then reads the wall clock. Later
// calls, including ones concurrent with the hook, wait for it to finish.
func (c *HookedClock) Now() time.Time {
	c.once.Do(c.hook)
	return time.Now()
}
