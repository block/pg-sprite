package decode_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/decode"
)

// reportDeadline bounds the whole report, so a server that has stopped
// answering cannot turn a deadline failure into a hang.
const reportDeadline = 5 * time.Second

// walsenderReport describes the server side of a stream that went quiet,
// for the failure message of a test that gave up waiting on it. It never
// fails the test itself: a query that errors or returns nothing is written
// into the report as such, because the report is read after the test has
// already failed.
//
// How to read it:
//
//   - The walsender in pg_stat_activity waiting on WalSenderWaitForWal while
//     the server's flush position is past its sent position: the walsender
//     was not woken for the WAL it should be sending and is sleeping until
//     its wal_sender_timeout tick.
//   - No pg_stat_replication row for the slot: the walsender has not
//     finished starting, so nothing has been decoded yet.
//   - server_wal_end ahead of delivered on the client: the walsender has
//     been sending keepalives but no data, so it is awake and the WAL it
//     reports holds nothing the slot's publication selects.
//   - A slot that is not active: the walsender is gone and the stream will
//     fail on its next read rather than stay quiet.
func (f slotFixture) walsenderReport(t *testing.T, stream *decode.Stream) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), reportDeadline)
	defer cancel()

	var out strings.Builder
	fmt.Fprintf(&out, "client: start=%s delivered=%s server_wal_end=%s\n",
		stream.Start(), stream.Delivered(), stream.ServerWALEnd())
	describeRows(ctx, &out, f.pool, "server", `
		SELECT pg_current_wal_lsn()::text AS current_lsn,
		       pg_current_wal_flush_lsn()::text AS flush_lsn,
		       current_setting('wal_sender_timeout') AS wal_sender_timeout`)
	describeRows(ctx, &out, f.pool, "slot", `
		SELECT active, active_pid, restart_lsn::text, confirmed_flush_lsn::text, wal_status
		FROM pg_replication_slots
		WHERE slot_name = $1`, f.target.DecodingName())
	describeRows(ctx, &out, f.pool, "walsender", `
		SELECT pid, state, wait_event_type, wait_event,
		       (now() - state_change)::text AS in_state_for,
		       left(query, 60) AS query
		FROM pg_stat_activity
		WHERE backend_type = 'walsender' AND datname = current_database()`)
	describeRows(ctx, &out, f.pool, "replication", `
		SELECT pid, state, sent_lsn::text, write_lsn::text, flush_lsn::text, replay_lsn::text,
		       reply_time::text
		FROM pg_stat_replication`)
	return out.String()
}

// describeRows writes every row of a query as one "label: col=value …" line,
// or the query's error, or "label: no rows". Columns come from the result
// so a new query needs no scanning code.
func describeRows(ctx context.Context, out *strings.Builder, pool *pgxpool.Pool, label, sql string, args ...any) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		fmt.Fprintf(out, "%s: query failed: %v\n", label, err)
		return
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	n := 0
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			fmt.Fprintf(out, "%s: read row: %v\n", label, err)
			return
		}
		n++
		fmt.Fprintf(out, "%s:", label)
		for i, v := range values {
			fmt.Fprintf(out, " %s=%v", fields[i].Name, v)
		}
		out.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(out, "%s: rows failed: %v\n", label, err)
		return
	}
	if n == 0 {
		fmt.Fprintf(out, "%s: no rows\n", label)
	}
}
