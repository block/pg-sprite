package decode

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// closeDeadline bounds how long a test waits for Close to return before
// it reports Close as held.
const closeDeadline = 10 * time.Second

// pipeConn returns a PgConn over net.Pipe to a peer that completes the
// startup handshake and then hands its side of the pipe to serve, and
// that side, so a test can hang up. A pipe has no send buffer, so a peer
// that stops reading holds the next write at once: the state a real
// server reaches only once the socket's send buffer has filled, which no
// test against a server can bring about promptly. At cleanup the peer is
// stopped, its side closed, and the peer waited for.
func pipeConn(t *testing.T, serve func(server net.Conn, stop <-chan struct{})) (*pgconn.PgConn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	stop := make(chan struct{})
	var peer sync.WaitGroup
	peer.Go(func() {
		backend := pgproto3.NewBackend(server, server)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			return
		}
		serve(server, stop)
	})
	t.Cleanup(func() {
		close(stop)
		assert.NoError(t, server.Close())
		peer.Wait()
	})
	cfg, err := pgconn.ParseConfig("postgres://u@pipe/db?sslmode=disable")
	require.NoError(t, err)
	cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) { return client, nil }
	cfg.LookupFunc = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	conn, err := pgconn.ConnectConfig(t.Context(), cfg)
	require.NoError(t, err)
	return conn, server
}

// discard reads and drops everything the stream sends until the pipe
// closes.
func discard(server net.Conn, _ <-chan struct{}) {
	_, _ = io.Copy(io.Discard, server)
}

// stopReading reads nothing more until the peer is stopped, so the
// stream's next write waits until then.
func stopReading(_ net.Conn, stop <-chan struct{}) {
	<-stop
}

// fixedClock is a clock that reads one instant.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// The heartbeat reports at a quarter of the server's wal_sender_timeout,
// never faster than its floor, and at its default on a server whose
// timeout is off.
func TestHeartbeatIntervalFollowsTheSenderTimeout(t *testing.T) {
	assert.Equal(t, 15*time.Second, heartbeatInterval(time.Minute))
	assert.Equal(t, 500*time.Millisecond, heartbeatInterval(2*time.Second))
	assert.Equal(t, minHeartbeatInterval, heartbeatInterval(time.Millisecond))
	assert.Equal(t, defaultHeartbeatInterval, heartbeatInterval(0))
}

// A stream that reported within the interval is left alone: the heartbeat
// does not touch the connection, which this stream does not have, and
// keeps running.
func TestHeartbeatSkipsAStreamThatReportedWithinTheInterval(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Stream{clock: fixedClock{now: now}, lastReport: now.Add(-499 * time.Millisecond)}

	assert.True(t, s.reportIfQuiet(t.Context(), 500*time.Millisecond))
}

// A stream that has failed stops its heartbeat without a report: the
// connection is no longer one to write to.
func TestHeartbeatStopsOnAFailedStream(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Stream{clock: fixedClock{now: now}, lastReport: now.Add(-time.Hour), failed: errors.New("receive failed")}

	assert.False(t, s.reportIfQuiet(t.Context(), 500*time.Millisecond))
}

// A report the connection cannot carry fails the stream, and that failure
// is what the owner's next call returns, not whatever the connection says
// when the owner next reads it.
func TestHeartbeatReportFailureFailsTheStream(t *testing.T) {
	conn, server := pipeConn(t, discard)
	require.NoError(t, server.Close())
	now := time.Unix(1000, 0)
	s := &Stream{conn: conn, slotName: "slot", clock: fixedClock{now: now}, lastReport: now.Add(-time.Hour)}

	assert.False(t, s.reportIfQuiet(t.Context(), 500*time.Millisecond), "a report that cannot be sent stops the heartbeat")

	_, err := s.Next(t.Context(), time.Millisecond)
	require.Error(t, err)
	assert.ErrorContains(t, err, "report 0/0 to slot slot")
	assert.ErrorIs(t, err, io.ErrClosedPipe)
	assert.ErrorIs(t, s.Confirm(t.Context(), 0), err)
}

// Close returns only once the heartbeat's goroutine has.
func TestCloseStopsTheHeartbeat(t *testing.T) {
	conn, _ := pipeConn(t, discard)
	now := time.Unix(1000, 0)
	s := &Stream{conn: conn, slotName: "slot", clock: fixedClock{now: now}, lastReport: now}
	s.startHeartbeat(t.Context(), time.Hour)

	require.NoError(t, s.Close(t.Context()))

	select {
	case <-s.heartbeatDone:
	default:
		t.Fatal("Close returned with the heartbeat still running")
	}
}

// A heartbeat report a server that has stopped reading holds in its write
// holds Close only until Close's context ends: Close then closes the socket
// under the write, the report fails, the heartbeat returns, and Close
// returns the context's cause.
func TestCloseEndsAHeartbeatReportTheServerDoesNotRead(t *testing.T) {
	conn, _ := pipeConn(t, stopReading)
	// No report has left the stream, so the first check sends one, and
	// the peer never takes it.
	s := &Stream{conn: conn, slotName: "slot", clock: progress.WallClock{}}
	s.startHeartbeat(t.Context(), minHeartbeatInterval)
	require.Eventually(t, func() bool {
		if s.mu.TryLock() {
			s.mu.Unlock()
			return false
		}
		return true
	}, closeDeadline, time.Millisecond, "the heartbeat never took the connection to report")

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	var closer sync.WaitGroup
	closer.Go(func() { closed <- s.Close(ctx) })
	select {
	case err := <-closed:
		closer.Wait()
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.ErrorContains(t, err, "stop heartbeat on slot slot")
	case <-time.After(closeDeadline):
		t.Fatal("Close stayed behind the heartbeat's report past its context")
	}

	select {
	case <-s.heartbeatDone:
	default:
		t.Fatal("Close returned with the heartbeat still running")
	}
	assert.ErrorIs(t, s.failed, io.ErrClosedPipe, "the report the socket was closed under fails the stream")
}
