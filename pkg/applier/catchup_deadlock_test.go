package applier

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

var deadlockTestNow = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// A lag stop inside a deadlocked batch's backoff carries the count in a
// row and says the batch was backing off, through the wrap the catch-up
// puts on it.
func TestRecordDeadlocksCarriesABackoffInProgressOntoTheLagStop(t *testing.T) {
	lag := &decode.SlotLagExceededError{Slot: "pgsprite_0123abcd", Cause: decode.SlotLagStreamBehind}
	wrapped := fmt.Errorf("catch-up on public.orders: %w", lag)

	got := recordDeadlocks(wrapped, 6, deadlockTestNow.Add(time.Second), deadlockTestNow)
	assert.Same(t, wrapped, got, "the error is returned as it came")
	assert.Equal(t, decode.CatchupDeadlocks{Consecutive: 6, BackingOff: true}, lag.Deadlocks)
}

// Once the backoff has passed, the batch is no longer waiting: a stop then
// still counts the deadlocks in a row but is not backing off.
func TestRecordDeadlocksAfterTheBackoffHasPassed(t *testing.T) {
	lag := &decode.SlotLagExceededError{Slot: "pgsprite_0123abcd", Cause: decode.SlotLagStreamBehind}
	require.Error(t, recordDeadlocks(lag, 2, deadlockTestNow, deadlockTestNow))
	assert.Equal(t, decode.CatchupDeadlocks{Consecutive: 2}, lag.Deadlocks)
}

// With no deadlock in progress the field stays zero, and an error that is
// not a lag stop passes through untouched.
func TestRecordDeadlocksWithoutADeadlockLeavesTheZeroValue(t *testing.T) {
	lag := &decode.SlotLagExceededError{Slot: "pgsprite_0123abcd", Cause: decode.SlotLagWALRetained}
	require.Error(t, recordDeadlocks(lag, 0, time.Time{}, deadlockTestNow))
	assert.Zero(t, lag.Deadlocks)

	lost := &decode.SlotLostError{Slot: "pgsprite_0123abcd"}
	assert.Same(t, error(lost), recordDeadlocks(lost, 3, deadlockTestNow.Add(time.Second), deadlockTestNow))
}
