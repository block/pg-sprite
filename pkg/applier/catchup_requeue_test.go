package applier

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loggedRequeues is the records logRequeue writes for one cycle, decoded
// from JSON so the test reads attributes, not message text.
func loggedRequeues(t *testing.T, outcome cycleOutcome, status Status) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	c := &Catchup{
		opts:    CatchupOptions{DeadlockAttempts: 10, Logger: slog.New(slog.NewJSONHandler(&out, nil))},
		flusher: &Flusher{},
	}
	c.logRequeue(outcome, status)
	var records []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal(line, &rec))
		records = append(records, rec)
	}
	return records
}

// A refused batch is logged as the run's count reaches each power of two,
// and not on the cycles between, so a long wait for the copy writes a few
// records rather than one a cycle.
func TestLogRequeueLogsARefusedBatchAsItsCountDoubles(t *testing.T) {
	deferred := cycleOutcome{deferred: true, batch: Batch{Entries: make([]Entry, 3)}}
	for count, logged := range map[uint64]bool{1: true, 2: true, 3: false, 4: true, 5: false, 7: false, 8: true, 1024: true, 1025: false} {
		records := loggedRequeues(t, deferred, Status{BatchesDeferred: count})
		if !logged {
			assert.Empty(t, records, "refusal %d is between two powers of two", count)
			continue
		}
		require.Len(t, records, 1, "refusal %d", count)
		assert.Equal(t, "INFO", records[0]["level"])
		assert.EqualValues(t, count, records[0]["batches_deferred"])
		assert.EqualValues(t, 3, records[0]["entries"])
	}
}

// A cycle that flushed or paused logs nothing.
func TestLogRequeueIsSilentWithoutARequeue(t *testing.T) {
	assert.Empty(t, loggedRequeues(t, cycleOutcome{}, Status{BatchesDeferred: 1, BatchesDeadlocked: 1}))
	assert.Empty(t, loggedRequeues(t, cycleOutcome{paused: true}, Status{BatchesDeferred: 1, BatchesDeadlocked: 1}))
}
