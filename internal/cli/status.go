package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// run reports the engine's live database sessions. Phase 1 has no durable
// migration state — a change either committed within its budgets or was
// refused — so status is a view over pg_stat_activity for pg-sprite sessions.
func (c *StatusCmd) run(ctx context.Context, out io.Writer) error {
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `
		SELECT pid, state,
		       COALESCE(wait_event_type || '/' || wait_event, '-'),
		       COALESCE(now() - query_start, '0'::interval)::text,
		       left(query, 80)
		FROM pg_stat_activity
		WHERE application_name = 'pg-sprite' AND pid <> pg_backend_pid()
		ORDER BY query_start`)
	if err != nil {
		return fmt.Errorf("query pg_stat_activity: %w", err)
	}
	defer rows.Close()

	sessions := 0
	for rows.Next() {
		var pid int
		var state, waitEvent, runningFor, query string
		if err := rows.Scan(&pid, &state, &waitEvent, &runningFor, &query); err != nil {
			return fmt.Errorf("scan pg_stat_activity row: %w", err)
		}
		sessions++
		if sessions == 1 {
			if _, err := fmt.Fprintln(out, "active pg-sprite sessions:"); err != nil {
				return fmt.Errorf("write status: %w", err)
			}
		}
		if _, err := fmt.Fprintf(out, "  pid=%d state=%s wait=%s running_for=%s query=%q\n",
			pid, state, waitEvent, runningFor, query); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read pg_stat_activity: %w", err)
	}
	if sessions == 0 {
		if _, err := fmt.Fprintln(out, "no active pg-sprite sessions (Phase 1 keeps no durable migration state: a change either committed within its budgets or was refused)"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
	}
	return nil
}
