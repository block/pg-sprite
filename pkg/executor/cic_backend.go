package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// awaitBackendStopped waits until the given backend provably stopped
// executing: it disconnected, or pg_stat_activity positively reports it
// idle (in or out of a transaction). It is the catalog verdict's first
// proof — a client-side failure (cancellation, connection loss) returns
// before the server acts on it, and an inspection racing the still-running
// build could wrongly report a clean catalog while the build goes on to
// create its invalid entry. Anything short of positive evidence fails
// closed: a state that means the statement may still be running keeps
// polling until ctx expires, and a state with no visibility (activity
// tracking disabled, hidden backend) refuses immediately — it will never
// become provable by waiting.
func awaitBackendStopped(ctx context.Context, q querier, pid uint32) error {
	const pollInterval = 50 * time.Millisecond
	for {
		var state *string
		err := q.QueryRow(ctx,
			`SELECT state FROM pg_catalog.pg_stat_activity WHERE pid OPERATOR(pg_catalog.=) $1`,
			int64(pid)).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			// The backend is gone; a disconnected backend runs nothing.
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect build backend %d: %w", pid, err)
		}
		switch classifyBackendState(state) {
		case backendStopped:
			return nil
		case backendRunning:
			// The statement may still be running; keep polling.
		case backendUnprovable:
			return fmt.Errorf("build backend %d reports state %s: cannot prove the statement stopped", pid, describeState(state))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("build backend %d is still executing: %w", pid, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// backendVerdict is the classification of one pg_stat_activity state
// observation.
type backendVerdict int

const (
	// backendStopped means the backend has positively stopped executing.
	backendStopped backendVerdict = iota
	// backendRunning means the statement may still be executing; the
	// observation is worth repeating.
	backendRunning
	// backendUnprovable means no amount of waiting turns the observation
	// into proof: activity tracking is off, the state is hidden, or the
	// state is one this executor does not know.
	backendUnprovable
)

// classifyBackendState maps one observed pg_stat_activity state to its
// verdict. The cases are the documented pg_stat_activity.state vocabulary
// (PostgreSQL 14–18): the idle states, "active" and "fastpath function
// call" (may still be executing), NULL (hidden backend), and "disabled"
// (track_activities off). Only positive evidence counts as stopped;
// NULL, "disabled", and any state outside the vocabulary are unprovable —
// treating them as stopped would let the verdict race a build that is in
// fact still running.
func classifyBackendState(state *string) backendVerdict {
	if state == nil {
		return backendUnprovable
	}
	switch *state {
	case "idle", "idle in transaction", "idle in transaction (aborted)":
		return backendStopped
	case "active", "fastpath function call":
		return backendRunning
	default:
		return backendUnprovable
	}
}

// describeState renders an observed state for an error message.
func describeState(state *string) string {
	if state == nil {
		return "NULL"
	}
	return strconv.Quote(*state)
}

// acquireBudgetedSession acquires one pooled session and applies the
// CONCURRENTLY wait policy to it. CONCURRENTLY statements refuse to run
// inside a transaction block, which also rules out SET LOCAL, so the
// overrides are session-level: the returned release resets them before the
// session goes back to the pool and discards the session when the reset
// cannot be proven.
