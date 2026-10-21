package dbconn

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrInvalidTimeout reports a session timeout the server cannot hold as asked.
// PostgreSQL counts these settings in whole milliseconds, reads 0 as no
// timeout, and holds at most a 32-bit count. A duration below one millisecond
// would be sent as 0 and leave the session unbounded; one above the maximum
// would be refused by the server at connect.
var ErrInvalidTimeout = errors.New("invalid session timeout")

// maxTimeout is the longest lock_timeout or statement_timeout PostgreSQL can
// hold: its settings are a 32-bit count of milliseconds, about 24.8 days.
const maxTimeout = time.Duration(math.MaxInt32) * time.Millisecond

// validateTimeouts refuses a LockTimeout or StatementTimeout that is neither
// unset (zero, meaning the package default) nor between one millisecond and
// maxTimeout. A duration that is not a whole number of milliseconds truncates
// toward zero but never to it, so it stays a bound.
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
		if timeout.value > maxTimeout {
			return fmt.Errorf("%w: %s %s is above the %s PostgreSQL can hold",
				ErrInvalidTimeout, timeout.field, timeout.value, maxTimeout)
		}
	}
	return nil
}
