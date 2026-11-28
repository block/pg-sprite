package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// statusFormatVersion identifies the status report's JSON shape. It is
// bumped whenever a field is added, removed, or changes meaning; a consumer
// must reject a version it does not know.
const statusFormatVersion = 1

// statusReport is the status command's output: the engine's live sessions
// on the connected database, and every copy-and-swap run there with a
// checkpoint row in a non-terminal phase. Both lists are always arrays,
// empty when nothing is in them.
type statusReport struct {
	FormatVersion int       `json:"format_version"`
	Sessions      []session `json:"sessions"`
	// CheckpointTable says whether StoppedRuns could be read: a run is
	// listed only from a readable checkpoint table.
	CheckpointTable checkpointTableState `json:"checkpoint_table"`
	// StoppedRuns are the runs with a checkpoint row in a non-terminal
	// phase, in table order. A run in progress is listed too, with Running
	// set; every other one is stopped, and holds what it left until the
	// same statement resumes it or a discard drops it.
	StoppedRuns []statusRun `json:"stopped_runs"`
}

// session is one live pg-sprite backend from pg_stat_activity.
type session struct {
	PID        int    `json:"pid"`
	State      string `json:"state"`
	WaitEvent  string `json:"wait_event"`
	RunningFor string `json:"running_for"`
	Query      string `json:"query"`
}

// run reports the engine's live database sessions and the copy-and-swap
// runs whose checkpoint rows are still in flight. Nothing is written: a
// database where no run ever ran has no checkpoint table, which is
// reported, not created.
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
	state, runs, err := readStatusRuns(ctx, pool)
	if err != nil {
		return err
	}
	report := statusReport{
		FormatVersion:   statusFormatVersion,
		Sessions:        sessions,
		CheckpointTable: state,
		StoppedRuns:     runs,
	}
	if c.JSON {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode status: %w", err)
		}
		if _, err := fmt.Fprintln(out, string(b)); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		return nil
	}
	if _, err := io.WriteString(out, renderStatus(report)); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	return nil
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
