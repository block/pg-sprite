package dbconn

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidTimeout reports a session timeout the server cannot hold as asked.
// PostgreSQL counts these settings in whole milliseconds and reads 0 as no
// timeout, so a duration below one millisecond would be sent as 0 and leave
// the session unbounded.
var ErrInvalidTimeout = errors.New("invalid session timeout")

// validateTimeouts refuses a LockTimeout or StatementTimeout that is neither
// unset (zero, meaning the package default) nor at least one millisecond. A
// longer duration that is not a whole number of milliseconds truncates toward
// zero but never to it, so it stays a bound.
//
// INV: LK-2 — a session timeout that reaches the server as 0 is no bound at
// all, so it is refused here rather than applied.
func validateTimeouts(cfg Config) error {
	for _, timeout := range []struct {
		field string
		value time.Duration
	}{{"LockTimeout", cfg.LockTimeout}, {"StatementTimeout", cfg.StatementTimeout}} {
		if timeout.value != 0 && timeout.value < time.Millisecond {
			return fmt.Errorf("%w: %s %s is below PostgreSQL's one-millisecond resolution; use at least 1ms, or zero for the default",
				ErrInvalidTimeout, timeout.field, timeout.value)
		}
	}
	return nil
}
