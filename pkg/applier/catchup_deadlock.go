package applier

import (
	"errors"
	"time"

	"github.com/block/pg-sprite/pkg/decode"
)

// recordDeadlocks carries the catch-up's run of deadlocked flushes onto a
// slot lag ceiling stop, so the stop names the deadlocks beside the slot:
// a batch waiting out its backoff holds the confirmation below it, and the
// ceiling can end the run before the deadlocks use up their attempts.
// deadlocks is the count in a row, and the batch is backing off while now
// is before retryAt. err is returned as it came.
func recordDeadlocks(err error, deadlocks int, retryAt, now time.Time) error {
	if lag, ok := errors.AsType[*decode.SlotLagExceededError](err); ok {
		lag.Deadlocks = decode.CatchupDeadlocks{Consecutive: deadlocks, BackingOff: now.Before(retryAt)}
	}
	return err
}
