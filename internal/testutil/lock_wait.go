package testutil

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BlockedBy reports whether some session is waiting on a lock held by the
// session with backend process ID holder. A test that holds a lock and
// starts the code under test waits on this before acting on the wait: it
// names the holder, so another test's lock wait on a shared server, in any
// database, never satisfies it, and the holder's own granted locks do not
// either.
func BlockedBy(ctx context.Context, pool *pgxpool.Pool, holder uint32) (bool, error) {
	var blocked bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_stat_activity
			WHERE $1::pg_catalog.int4 OPERATOR(pg_catalog.=) ANY (pg_catalog.pg_blocking_pids(pid))
		)`, int64(holder)).Scan(&blocked)
	return blocked, err
}
