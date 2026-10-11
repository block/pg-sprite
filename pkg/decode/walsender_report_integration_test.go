package decode_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

// reportDeadline bounds the whole report, so a server that has stopped
// answering cannot turn a deadline failure into a hang.
const reportDeadline = 5 * time.Second

// reportGuide is the report's first line: where a reader of a CI failure
// finds the guide below.
const reportGuide = "see walsenderReport in pkg/decode/walsender_report_integration_test.go for how to read this"

// walsenderReport describes the server side of a stream that went quiet,
// for the failure message of a test that gave up waiting on it. It never
// fails the test itself: a query that errors or returns nothing is written
// into the report as such, because the report is read after the test has
// already failed.
//
// The replication line is the walsender holding the stream's slot; the
// walsender lines are every walsender in the database, holds_slot marking
// that one. How to read it:
//
//   - replication: unsent_bytes above zero while the walsender waits on
//     WalSenderWaitForWAL (spelled WalSenderWaitForWal on newer majors): the
//     server has flushed WAL the walsender has not sent. It was not woken
//     for that WAL and sleeps until its wal_sender_timeout tick. The wait
//     event alone says nothing — an idle, caught-up walsender waits there
//     too; the flush-to-sent gap is the signal.
//   - replication: no rows: no walsender holds the slot. A state other than
//     streaming: the walsender is still starting or catching up and has
//     decoded nothing for the client yet.
//   - client: delivered equal to server_wal_end, past start and at or past
//     the server's flush position, with unsent_bytes zero: the walsender is
//     awake and has been sending keepalives but no data — the WAL it
//     reports holds nothing the slot's publication selects. server_wal_end
//     ahead of delivered means the stream is inside a transaction, after
//     its BEGIN and before its COMMIT.
//   - slot: active=false: the walsender is gone and the stream will fail on
//     its next read rather than stay quiet.
//   - walsender: in_state_for is the time since the walsender entered its
//     current state (START_REPLICATION, for a streaming one), not how long
//     it has been asleep.
func (f slotFixture) walsenderReport(t *testing.T, stream *decode.Stream) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), reportDeadline)
	defer cancel()

	var out strings.Builder
	out.WriteString(reportGuide + "\n")
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
		SELECT a.pid, a.state, a.wait_event_type, a.wait_event,
		       (now() - a.state_change)::text AS in_state_for,
		       s.active_pid IS NOT NULL AS holds_slot,
		       left(a.query, 60) AS query
		FROM pg_stat_activity AS a
		LEFT JOIN pg_replication_slots AS s ON s.slot_name = $1 AND s.active_pid = a.pid
		WHERE a.backend_type = 'walsender' AND a.datname = current_database()`, f.target.DecodingName())
	describeRows(ctx, &out, f.pool, "replication", `
		SELECT r.pid, r.state, r.sent_lsn::text,
		       pg_current_wal_flush_lsn()::text AS server_flush_lsn,
		       pg_wal_lsn_diff(pg_current_wal_flush_lsn(), r.sent_lsn)::bigint AS unsent_bytes,
		       r.write_lsn::text, r.flush_lsn::text, r.reply_time::text
		FROM pg_stat_replication AS r
		JOIN pg_replication_slots AS s ON s.active_pid = r.pid
		WHERE s.slot_name = $1`, f.target.DecodingName())
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

// reportLines returns the report's lines that carry the label.
func reportLines(report, label string) []string {
	var lines []string
	for line := range strings.SplitSeq(report, "\n") {
		if strings.HasPrefix(line, label+":") {
			lines = append(lines, line)
		}
	}
	return lines
}

// The report describes the walsender holding the stream's slot, not every
// walsender in the database: the slot's own creating session is a second
// walsender, and other tests' streams share the server.
func TestWalsenderReportDescribesTheStreamsWalsender(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	var holder int32
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT active_pid FROM pg_replication_slots WHERE slot_name = $1`, slot.Name()).Scan(&holder))

	report := f.walsenderReport(t, stream.Stream)
	assert.True(t, strings.HasPrefix(report, reportGuide+"\n"), "the report opens with the pointer to its guide: %s", report)

	replication := reportLines(report, "replication")
	require.Len(t, replication, 1, "one row, for the slot's walsender: %s", report)
	assert.Contains(t, replication[0], fmt.Sprintf("pid=%d ", holder))
	assert.Contains(t, replication[0], "state=streaming")
	assert.Contains(t, replication[0], "unsent_bytes=")

	var holding []string
	for _, line := range reportLines(report, "walsender") {
		if strings.Contains(line, "holds_slot=true") {
			holding = append(holding, line)
		}
	}
	require.Len(t, holding, 1, "one walsender marked as holding the slot: %s", report)
	assert.Contains(t, holding[0], fmt.Sprintf("pid=%d ", holder))
}
