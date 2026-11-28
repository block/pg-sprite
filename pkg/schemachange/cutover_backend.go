package schemachange

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// swapBackend identifies the server process running one swap attempt. The
// pid alone is not an identity — the server reuses pids — so the backend's
// start time is carried with it.
type swapBackend struct {
	pid     uint32
	started time.Time
}

// backendExitPoll is how often the catalog is asked whether a lost
// attempt's backend has exited.
const backendExitPoll = 50 * time.Millisecond

// readSwapBackend records which backend the swap transaction runs on, so a
// lost attempt can be followed to its end on the server. A session that
// cannot read its own start, because it logged in as one role and runs as
// another without pg_read_all_stats, cannot be followed, so the attempt is
// refused before it locks anything with an error wrapping
// dbconn.ErrSessionStartHidden that names both roles.
func readSwapBackend(ctx context.Context, tx pgx.Tx) (swapBackend, error) {
	var pid uint32
	var started *time.Time
	var sessionRole, currentRole string
	// INV: CO-9
	err := tx.QueryRow(ctx, `
		SELECT pid, backend_start, session_user, current_user
		FROM pg_catalog.pg_stat_activity
		WHERE pid OPERATOR(pg_catalog.=) pg_catalog.pg_backend_pid()`).Scan(&pid, &started, &sessionRole, &currentRole)
	if err != nil {
		return swapBackend{}, fmt.Errorf("read cutover backend: %w", err)
	}
	// INV: LK-4 — a lost attempt is followed by its PID and start together;
	// a PID alone may name a later backend.
	if started == nil {
		return swapBackend{}, fmt.Errorf("%w: cutover backend %d logged in as %q and runs as %q",
			dbconn.ErrSessionStartHidden, pid, sessionRole, currentRole)
	}
	return swapBackend{pid: pid, started: *started}, nil
}

// awaitBackendExit waits until the attempt's backend has left
// pg_stat_activity, so the catalog read that follows sees the attempt's
// final state rather than a transaction still deciding (LK-4). A client that
// stopped hearing from the server knows nothing about where the backend is:
// it may be inside COMMIT behind a synchronous standby or a deferred
// trigger, and a fresh connection cannot see a rename that has not
// committed. The wait is bounded by the attempt's own lock_timeout and
// statement_timeout — the longest the server lets any statement of the
// attempt run — counted in polls of the catalog, with sleep between them.
// A backend still present when the bound runs out is reported as such;
// the caller refuses rather than guesses.
func awaitBackendExit(ctx context.Context, pool *pgxpool.Pool, backend swapBackend, bound time.Duration, sleep func(context.Context, time.Duration) error) error {
	polls := int(bound/backendExitPoll) + 1
	for range polls {
		present, err := backendPresent(ctx, pool, backend)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		if err := sleep(ctx, backendExitPoll); err != nil {
			return err
		}
	}
	return fmt.Errorf("backend %d of the lost cutover attempt is still running after %s", backend.pid, bound)
}

// backendPresent reports whether the attempt's backend — the same pid
// started at the same time — is still registered with the server.
func backendPresent(ctx context.Context, pool *pgxpool.Pool, backend swapBackend) (bool, error) {
	var present bool
	// INV: CO-9
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_stat_activity
			WHERE pid OPERATOR(pg_catalog.=) $1 AND backend_start OPERATOR(pg_catalog.=) $2)`, backend.pid, backend.started).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("look up backend %d of the lost cutover attempt: %w", backend.pid, err)
	}
	return present, nil
}
