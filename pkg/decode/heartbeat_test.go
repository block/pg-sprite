package decode

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

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
