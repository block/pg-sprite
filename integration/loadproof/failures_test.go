package loadproof_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/internal/testutil"
)

// The SQLSTATEs the swap is allowed to cause in a load statement: one that
// queues behind the swap's lock past its lock_timeout fails with
// lockTimeoutCode, and one a connection had prepared before the swap fails
// once with staleStatementCode when its result type changed under it, after
// which the driver re-prepares. Any other code is a failure the proof
// reports as its own.
const (
	lockTimeoutCode    = "55P03"
	staleStatementCode = "0A000"
)

// swapWindow is when the swap can have caused a load failure. It opens at
// the mark, the last instant the poll saw the row say the swap had not
// begun: the cutover is saved, the lock taken, and the key retyped only
// after it. It closes at the binary's exit, which follows the swap's
// commit.
type swapWindow struct {
	opened time.Time
	closed time.Time
}

// failureAllowed reports whether a recorded load failure is one the swap is
// allowed to cause, at the time it is allowed to cause it. A failure before
// the window opens happened while the swap had not begun, so none is
// allowed. A stale prepared statement follows the retype for as long as the
// load runs, because each pooled connection meets it the first time it
// reuses the statement. A lock timeout, or a statement that ran out its own
// client-side deadline while the swap held the lock, is allowed until that
// deadline has passed after the binary's exit, the longest a statement
// queued behind the swap can take to report it.
func failureAllowed(f testutil.Failure, w swapWindow) bool {
	if f.At.Before(w.opened) {
		return false
	}
	if f.Code == staleStatementCode {
		return true
	}
	if !waitedOnTheSwapLock(f) {
		return false
	}
	return !f.At.After(w.closed.Add(testutil.MutationTimeout))
}

// waitedOnTheSwapLock reports whether a failure is how a statement that
// queued behind the swap's lock ends: a lock timeout on the server, or the
// statement's own client-side deadline.
func waitedOnTheSwapLock(f testutil.Failure) bool {
	if errors.Is(f.Err, context.DeadlineExceeded) {
		return true
	}
	return f.Code == lockTimeoutCode
}

// failureAt builds a recorded failure at t: a server error with code, or a
// client-side deadline when code is empty.
func failureAt(at time.Time, code string) testutil.Failure {
	if code == "" {
		return testutil.Failure{At: at, Err: fmt.Errorf("update: %w", context.DeadlineExceeded)}
	}
	return testutil.Failure{At: at, Code: code, Err: &pgconn.PgError{Code: code}}
}

// The swap can only cause a failure once it has begun, so every kind is
// refused before the window opens, however allowed it is inside.
func TestFailureAllowedRefusesEveryKindBeforeTheSwapBegins(t *testing.T) {
	opened := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w := swapWindow{opened: opened, closed: opened.Add(time.Minute)}
	before := opened.Add(-time.Millisecond)
	assert.False(t, failureAllowed(failureAt(before, lockTimeoutCode), w), "lock timeout during the copy")
	assert.False(t, failureAllowed(failureAt(before, staleStatementCode), w), "stale statement before the retype")
	assert.False(t, failureAllowed(failureAt(before, ""), w), "client deadline during the copy")
}

// Inside the window the swap's lock and the retype explain a lock timeout,
// a client deadline, and a stale prepared statement; any other code is not
// something the swap causes.
func TestFailureAllowedAcceptsTheSwapsOwnFailuresInsideTheWindow(t *testing.T) {
	opened := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w := swapWindow{opened: opened, closed: opened.Add(time.Minute)}
	inside := opened.Add(30 * time.Second)
	assert.True(t, failureAllowed(failureAt(inside, lockTimeoutCode), w), "lock timeout behind the swap")
	assert.True(t, failureAllowed(failureAt(inside, ""), w), "client deadline behind the swap")
	assert.True(t, failureAllowed(failureAt(inside, staleStatementCode), w), "stale statement after the retype")
	assert.False(t, failureAllowed(failureAt(inside, "23505"), w), "a unique violation is not the swap's")
}

// After the binary exits, the swap's lock is released: a statement queued
// behind it reports its lock failure within its own deadline, and a later
// one is not the swap's. A stale prepared statement stays allowed, because
// a pooled connection first reuses its statement whenever the load next
// hands it out.
func TestFailureAllowedClosesTheLockWindowOneDeadlineAfterExit(t *testing.T) {
	opened := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	closed := opened.Add(time.Minute)
	w := swapWindow{opened: opened, closed: closed}
	withinDeadline := closed.Add(testutil.MutationTimeout)
	pastDeadline := withinDeadline.Add(time.Millisecond)
	assert.True(t, failureAllowed(failureAt(withinDeadline, lockTimeoutCode), w))
	assert.True(t, failureAllowed(failureAt(withinDeadline, ""), w))
	assert.False(t, failureAllowed(failureAt(pastDeadline, lockTimeoutCode), w))
	assert.False(t, failureAllowed(failureAt(pastDeadline, ""), w))
	assert.True(t, failureAllowed(failureAt(closed.Add(time.Hour), staleStatementCode), w))
}
