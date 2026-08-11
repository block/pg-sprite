package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// session is one live pg-sprite backend from pg_stat_activity.
type session struct {
	PID        int    `json:"pid"`
	State      string `json:"state"`
	WaitEvent  string `json:"wait_event"`
	RunningFor string `json:"running_for"`
	Query      string `json:"query"`
}

// run reports the engine's live database sessions. Phase 1 has no durable
// schema-change state — a change either committed within its budgets or was
// refused — so status is a view over pg_stat_activity for pg-sprite sessions.
func (c *StatusCmd) run(ctx context.Context, out io.Writer) error {
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	sessions, err := querySessions(ctx, pool)
	if err != nil {
		return err
	}
	if c.JSON {
		b, err := json.MarshalIndent(sessions, "", "  ")
		if err != nil {
			return fmt.Errorf("encode status: %w", err)
		}
		if _, err := fmt.Fprintln(out, string(b)); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		return nil
	}
	return renderSessions(out, sessions)
}

// querySessions lists the live pg-sprite backends on the connected database,
// other than the one running the status query itself. pg_stat_activity is
// cluster-wide, but a schema change targets one database — sessions on other
// databases are another change's business. pg_stat_activity nulls out state
// and query for other roles' backends unless the viewer has
// pg_read_all_stats — exactly the read-only-operator-checking-on-the-engine-
// role shape — so those columns are coalesced instead of crashing the scan.
func querySessions(ctx context.Context, pool *pgxpool.Pool) ([]session, error) {
	rows, err := pool.Query(ctx, `
		SELECT pid,
		       COALESCE(state, '-'),
		       COALESCE(wait_event_type || '/' || wait_event, '-'),
		       COALESCE(now() - query_start, '0'::interval)::text,
		       COALESCE(left(query, 80), '<insufficient privilege>')
		FROM pg_stat_activity
		WHERE application_name = 'pg-sprite'
		  AND datname = current_database()
		  AND pid <> pg_backend_pid()
		ORDER BY query_start`)
	if err != nil {
		return nil, fmt.Errorf("query pg_stat_activity: %w", err)
	}
	defer rows.Close()

	sessions := []session{}
	for rows.Next() {
		var s session
		if err := rows.Scan(&s.PID, &s.State, &s.WaitEvent, &s.RunningFor, &s.Query); err != nil {
			return nil, fmt.Errorf("scan pg_stat_activity row: %w", err)
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	return sessions, nil
}

// renderSessions writes the human-readable session listing.
func renderSessions(out io.Writer, sessions []session) error {
	if len(sessions) == 0 {
		if _, err := fmt.Fprintln(out, "no active pg-sprite sessions (Phase 1 keeps no durable schema-change state: a change either committed within its budgets or was refused)"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintln(out, "active pg-sprite sessions:"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	for _, s := range sessions {
		if _, err := fmt.Fprintf(out, "  pid=%d state=%s wait=%s running_for=%s query=%q\n",
			s.PID, s.State, s.WaitEvent, s.RunningFor, s.Query); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
	}
	return nil
}
