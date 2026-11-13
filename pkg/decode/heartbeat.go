package decode

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// heartbeatFraction is how much of the server's wal_sender_timeout the
	// stream lets pass without a report before its heartbeat sends one. The
	// walsender asks for a reply at half the timeout and ends the stream at
	// the full timeout, counted from the last message it received; a report
	// at a quarter, checked at an eighth, reaches it within three eighths of
	// the timeout of the one before. The timeout is read once, when the
	// stream opens, so a stream already open keeps the interval it opened
	// with when a reload changes the setting.
	heartbeatFraction = 4
	// defaultHeartbeatInterval is the heartbeat's interval on a server whose
	// wal_sender_timeout is zero, which never ends a stream for silence: the
	// report still keeps the connection from looking idle to anything
	// between the stream and the server.
	defaultHeartbeatInterval = 10 * time.Second
	// minHeartbeatInterval is the shortest interval the heartbeat runs at,
	// so a server with a timeout of a few milliseconds does not turn the
	// heartbeat into a busy loop. A report then reaches the walsender
	// within one and a half floors of the one before, so a server whose
	// timeout is shorter than that can still end a stream its owner is
	// away from.
	minHeartbeatInterval = 10 * time.Millisecond
)

// readSenderTimeout reads wal_sender_timeout as the session conn is about
// to become a walsender sees it — a role or database setting included —
// in milliseconds, which is the setting's unit on every supported server.
func readSenderTimeout(ctx context.Context, conn *pgconn.PgConn) (time.Duration, error) {
	results, err := conn.Exec(ctx, `
		SELECT setting
		FROM pg_catalog.pg_settings
		WHERE name = 'wal_sender_timeout'`).ReadAll()
	if err != nil {
		return 0, fmt.Errorf("read wal_sender_timeout: %w", err)
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		return 0, fmt.Errorf("%w: read wal_sender_timeout: expected one value", ErrInvariantViolation)
	}
	ms, err := strconv.ParseInt(string(results[0].Rows[0][0]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read wal_sender_timeout: %w", err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// heartbeatInterval is how long the stream may go without a report before
// its heartbeat sends one, for a server whose wal_sender_timeout is
// timeout.
func heartbeatInterval(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultHeartbeatInterval
	}
	return max(timeout/heartbeatFraction, minHeartbeatInterval)
}

// startHeartbeat starts the goroutine that keeps the replication connection
// answered while the caller is away from Next: in a flush, waiting on a
// pool, or anywhere else the caller does not read the stream. The
// goroutine is the stream's own; Close stops it, and it stops by itself
// once the stream has failed. heartbeatDone closes when it has returned.
func (s *Stream) startHeartbeat(ctx context.Context, interval time.Duration) {
	s.stopHeartbeat = make(chan struct{})
	s.heartbeatDone = make(chan struct{})
	s.heartbeat.Go(func() {
		defer close(s.heartbeatDone)
		s.beat(ctx, interval)
	})
}

// beat checks, at half the interval, whether the stream has gone the
// interval without a report, and sends one when it has, until the
// heartbeat is stopped or the stream has failed.
func (s *Stream) beat(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval / 2)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopHeartbeat:
			return
		case <-ticker.C:
		}
		if !s.reportIfQuiet(ctx, interval) {
			return
		}
	}
}

// reportIfQuiet sends the confirmed position when no report has left the
// stream for the interval, and reports whether the stream is still
// running. A report the connection cannot carry ends the stream, as it
// does from Next or Confirm.
func (s *Stream) reportIfQuiet(ctx context.Context, interval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return false
	}
	if s.clock.Now().Sub(s.lastReport) < interval {
		return true
	}
	// INV: ST-4 — the heartbeat reports the position the caller confirmed
	// and nothing else, so the slot moves only on the caller's word.
	if err := s.sendStatus(ctx, s.confirmed); err != nil {
		s.failed = err
		return false
	}
	return true
}

// endHeartbeat stops the heartbeat and waits for its goroutine. A report
// in its write when the stop comes does not see the stop, and the library
// writes it without a context, so a server that has stopped reading holds
// it once the socket's send buffer is full. When ctx ends before the
// goroutine has returned, endHeartbeat closes the socket under the write,
// which fails it, waits for the goroutine to return, and returns ctx's
// cause. It is safe to call more than once and on a stream that never
// started one.
func (s *Stream) endHeartbeat(ctx context.Context) error {
	if s.stopHeartbeat == nil {
		return nil
	}
	s.stopOnce.Do(func() { close(s.stopHeartbeat) })
	defer s.heartbeat.Wait()
	select {
	case <-s.heartbeatDone:
		return nil
	case <-ctx.Done():
	}
	// A heartbeat that returned as ctx ended is not one held in its write.
	select {
	case <-s.heartbeatDone:
		return nil
	default:
	}
	closeErr := s.conn.Conn().Close()
	return errors.Join(fmt.Errorf("stop heartbeat on slot %s: %w", s.slotName, context.Cause(ctx)), closeErr)
}
