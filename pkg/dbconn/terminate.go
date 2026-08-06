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
func TerminateBlockers(ctx context.Context, q Querier, pid int) ([]int, error) {
	rows, err := q.Query(ctx, `
		SELECT b.pid
		  FROM unnest(pg_blocking_pids($1::int)) AS b(pid)
		 WHERE pg_terminate_backend(b.pid)`, pid)
	if err != nil {
		return nil, fmt.Errorf("terminate backends blocking pid %d: %w", pid, err)
	}
	terminated, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("collect terminated pids: %w", err)
	}
	return terminated, nil
}
