package dbconn

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the query surface TerminateBlockers needs; *pgxpool.Pool and
// *pgx.Conn both satisfy it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// TerminateBlockers terminates every backend currently blocking pid's lock
// acquisition (per pg_blocking_pids) and returns the pids it terminated.
//
// This is the bounded-cutover escape hatch: it targets only the backends
// standing in front of a specific waiting session (e.g. the cutover swap),
// never a broad sweep. Callers decide whether evicting those backends is
// acceptable; this function only does the targeted termination.
//
// The statement runs on whatever q the caller passes, under that session's
// own search_path — a pool not built by NewPool keeps a path that may list
// another schema ahead of pg_catalog — so every function it calls is
// qualified: a lookalike pg_terminate_backend(integer) that returns true and
// signals nothing would otherwise report the blocker evicted while it keeps
// the lock.
//
// INV: CO-9 — every name resolves to pg_catalog regardless of the session's path.
func TerminateBlockers(ctx context.Context, q Querier, pid int) ([]int, error) {
	rows, err := q.Query(ctx, `
		SELECT b.pid
		  FROM pg_catalog.unnest(pg_catalog.pg_blocking_pids($1::int)) AS b(pid)
		 WHERE pg_catalog.pg_terminate_backend(b.pid)`, pid)
	if err != nil {
		return nil, fmt.Errorf("terminate backends blocking pid %d: %w", pid, err)
	}
	terminated, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("collect terminated pids: %w", err)
	}
	return terminated, nil
}
