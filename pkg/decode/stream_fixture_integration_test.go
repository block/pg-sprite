package decode_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

const (
	// streamWait bounds one Next call; a quiet table yields a progress
	// delivery after it.
	streamWait = 200 * time.Millisecond
	// streamDeadline bounds how long a test waits for a change or a
	// position it has caused the server to produce.
	streamDeadline = 15 * time.Second
)

// openStream opens the route's stream from a position and arranges for it
// to be closed after the test.
func (f slotFixture) openStream(t *testing.T, from decode.LSN) *decode.Stream {
	t.Helper()
	stream, err := decode.OpenStream(t.Context(), f.cfg, f.pool, f.target, from)
	require.NoError(t, err)
	t.Cleanup(func() {
		// A test that already closed the stream leaves a closed connection
		// behind, whose second close is a no-op.
		if err := stream.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("close stream: %v", err)
		}
	})
	return stream
}

// nextChange reads deliveries until one carries a change, within the
// stream deadline.
func nextChange(t *testing.T, stream *decode.Stream) decode.ChangeEvent {
	t.Helper()
	return *nextChangeDelivery(t, stream).Change
}

// nextChangeDelivery reads deliveries until one carries a change, within
// the stream deadline, and returns the whole delivery.
func nextChangeDelivery(t *testing.T, stream *decode.Stream) decode.Delivery {
	t.Helper()
	deadline := time.Now().Add(streamDeadline)
	for time.Now().Before(deadline) {
		d, err := stream.Next(t.Context(), streamWait)
		require.NoError(t, err)
		if d.Change != nil {
			return d
		}
	}
	require.FailNow(t, "no change was delivered before the stream deadline")
	return decode.Delivery{}
}

// nextChanges reads n changes in stream order.
func nextChanges(t *testing.T, stream *decode.Stream, n int) []decode.ChangeEvent {
	t.Helper()
	changes := make([]decode.ChangeEvent, 0, n)
	for range n {
		changes = append(changes, nextChange(t, stream))
	}
	return changes
}

// assertSameChange asserts two streams decoded the same change. The
// delivered position a change arrives with belongs to the stream that
// delivered it — a keepalive between transactions raises it — so it is
// left out of the comparison.
func assertSameChange(t *testing.T, want, got decode.ChangeEvent) {
	t.Helper()
	want.Delivered, got.Delivered = 0, 0
	assert.Equal(t, want, got)
}

// nextError reads deliveries until the stream fails, within the stream
// deadline, and returns the error that ended it.
func nextError(t *testing.T, stream *decode.Stream) error {
	t.Helper()
	deadline := time.Now().Add(streamDeadline)
	for time.Now().Before(deadline) {
		if _, err := stream.Next(t.Context(), streamWait); err != nil {
			return err
		}
	}
	require.FailNow(t, "the stream did not fail before the stream deadline")
	return nil
}

// awaitDelivered reads progress until the stream has delivered at least the
// position, within the stream deadline; a change arriving meanwhile fails
// the test, since the caller expects a quiet table.
func awaitDelivered(t *testing.T, stream *decode.Stream, atLeast decode.LSN) {
	t.Helper()
	deadline := time.Now().Add(streamDeadline)
	for time.Now().Before(deadline) {
		d, err := stream.Next(t.Context(), streamWait)
		require.NoError(t, err)
		require.Nil(t, d.Change, "the table is expected to be quiet")
		if d.Delivered >= atLeast {
			return
		}
	}
	require.FailNow(t, "the stream did not deliver the position before the stream deadline",
		"wanted at least %s, delivered %s", atLeast, stream.Delivered())
}

// exec runs one statement on the fixture's pool.
func (f slotFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(sql, f.schema), args...)
	require.NoError(t, err)
}

// confirmedFlush is the position the server has recorded for the slot.
func (f slotFixture) confirmedFlush(t *testing.T, slot *decode.Slot) decode.LSN {
	t.Helper()
	status, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	require.True(t, found)
	return status.ConfirmedFlushLSN
}

// assertConfirmedFlushBecomes polls until the server's recorded position
// for the slot equals want; a status update is applied by the walsender
// after it is received, not synchronously with the client's send.
func (f slotFixture) assertConfirmedFlushBecomes(t *testing.T, slot *decode.Slot, want decode.LSN) {
	t.Helper()
	assert.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Equal(collect, want, f.confirmedFlush(t, slot))
	}, streamDeadline, 50*time.Millisecond)
}

// assertWALSenderFlushBecomes polls until the walsender holding the slot
// records want as the position the client last reported flushed, which
// proves the server has read a status update before the test asks what the
// server did with it.
func (f slotFixture) assertWALSenderFlushBecomes(t *testing.T, slot *decode.Slot, want decode.LSN) {
	t.Helper()
	assert.EventuallyWithT(t, func(collect *assert.CollectT) {
		var text *string
		err := f.pool.QueryRow(t.Context(), `
			SELECT r.flush_lsn::text
			FROM pg_catalog.pg_stat_replication AS r
			JOIN pg_catalog.pg_replication_slots AS s ON s.active_pid = r.pid
			WHERE s.slot_name = $1`, slot.Name()).Scan(&text)
		if !assert.NoError(collect, err) || !assert.NotNil(collect, text, "the walsender has read no status update") {
			return
		}
		got, err := decode.ParseLSN(*text)
		if assert.NoError(collect, err) {
			assert.Equal(collect, want, got)
		}
	}, streamDeadline, 50*time.Millisecond)
}

// column is one present text column of a decoded change.
func column(name, value string) decode.Column {
	return decode.Column{Name: name, Value: value, Present: true}
}

// absentColumn is a column pgoutput omitted as an unchanged TOAST value.
func absentColumn(name string) decode.Column {
	return decode.Column{Name: name, Present: false}
}
