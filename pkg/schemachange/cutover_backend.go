package schemachange

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
// lost attempt can be followed to its end on the server.
func readSwapBackend(ctx context.Context, tx pgx.Tx) (swapBackend, error) {
	var backend swapBackend
	err := tx.QueryRow(ctx, `
		SELECT pid, backend_start
		FROM pg_stat_activity
		WHERE pid = pg_backend_pid()`).Scan(&backend.pid, &backend.started)
	if err != nil {
		return swapBackend{}, fmt.Errorf("read cutover backend: %w", err)
	}
	return backend, nil
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
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE pid = $1 AND backend_start = $2)`, backend.pid, backend.started).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("look up backend %d of the lost cutover attempt: %w", backend.pid, err)
	}
	return present, nil
}
