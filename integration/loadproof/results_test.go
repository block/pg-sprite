package loadproof_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
)

// results is the run's record, written as results.json whether the proof
// passed or failed: the figures are what a reader compares across servers
// and versions, and a failed run's record is where its forensics start.
// Durations are reported, never asserted.
type results struct {
	Passed        bool        `json:"passed"`
	StartedAt     time.Time   `json:"started_at"`
	RunMillis     int64       `json:"run_ms"`
	ServerVersion string      `json:"server_version"`
	Binary        binaryInfo  `json:"binary"`
	Load          loadInfo    `json:"load"`
	Phases        []phaseInfo `json:"phases"`
	// SlotName is the replication slot the run used, so a leftover slot
	// after a failed run can be found.
	SlotName         string `json:"slot_name,omitempty"`
	PeakSlotLagBytes int64  `json:"peak_slot_lag_bytes"`
	// Verdict is the binary's stdout when it is JSON, as a verdict is;
	// Stdout holds it as text when it is not, a usage error or a crash,
	// so the record is written either way.
	Verdict json.RawMessage `json:"verdict,omitempty"`
	Stdout  string          `json:"stdout,omitempty"`
	Rows    rowsInfo        `json:"rows"`
}

// binaryInfo identifies the binary under proof.
type binaryInfo struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// loadInfo is the load's configuration and what it did.
type loadInfo struct {
	SeededRows     int     `json:"seeded_rows"`
	Workers        int     `json:"workers"`
	RatePerSecond  int     `json:"rate_per_second"`
	HotRowFraction float64 `json:"hot_row_fraction"`
	Inserts        int     `json:"inserts"`
	Updates        int     `json:"updates"`
	Deletes        int     `json:"deletes"`
	// Races counts mutations that lost an expected race with another
	// worker, the generator's own retries.
	Races int `json:"races"`
	// FailuresByCode counts every failed statement by SQLSTATE; a
	// client-side deadline is counted under "deadline".
	FailuresByCode map[string]int `json:"failures_by_code"`
	// MaxLatencyMillis is the slowest committed mutation.
	MaxLatencyMillis int64 `json:"max_latency_ms"`
	// CommittedAfterSwap counts mutations that committed after the binary
	// exited: the application writing the swapped-in table.
	CommittedAfterSwap int `json:"committed_after_swap"`
}

// phaseInfo is one checkpoint phase the poll saw, with when it was first
// seen relative to the binary's start and how long until the next phase
// was first seen (or the binary exited, for the last).
type phaseInfo struct {
	Phase           string `json:"phase"`
	FirstSeenMillis int64  `json:"first_seen_ms"`
	DurationMillis  int64  `json:"duration_ms"`
	// PeakSlotLagBytes is the most WAL the poll saw past the slot's
	// confirmed flush position while the row was in this phase.
	PeakSlotLagBytes int64 `json:"peak_slot_lag_bytes"`
}

// rowsInfo is what the row oracle covered.
type rowsInfo struct {
	// Compared is how many retained-source rows the oracle compared.
	Compared int64 `json:"compared"`
	// Excluded is how many keys the load touched after the mark and the
	// oracle therefore left out.
	Excluded int `json:"excluded"`
}

// The keys failures without a SQLSTATE are counted under in
// FailuresByCode, beside the SQLSTATEs.
const (
	deadlineCode = "deadline"
	otherCode    = "other"
)

func newResults(c config) *results {
	return &results{
		Binary: binaryInfo{Path: c.Binary},
		Load:   loadInfo{SeededRows: c.Rows, Workers: c.Workers, RatePerSecond: c.RatePerSecond, HotRowFraction: c.HotRowFraction, FailuresByCode: map[string]int{}},
	}
}

// recordWatch folds what the poll saw into the results, with phase
// times relative to started and the last phase lasting until now.
func (r *results) recordWatch(w watched, started time.Time) {
	r.SlotName = w.SlotName
	r.PeakSlotLagBytes = w.PeakSlotLagBytes
	phases := make([]checkpoint.Phase, 0, len(w.FirstSeen))
	for p := range w.FirstSeen {
		phases = append(phases, p)
	}
	slices.SortFunc(phases, func(a, b checkpoint.Phase) int { return w.FirstSeen[a].Compare(w.FirstSeen[b]) })
	ended := time.Now()
	for i, p := range phases {
		next := ended
		if i+1 < len(phases) {
			next = w.FirstSeen[phases[i+1]]
		}
		r.Phases = append(r.Phases, phaseInfo{
			Phase:            p.String(),
			FirstSeenMillis:  w.FirstSeen[p].Sub(started).Milliseconds(),
			DurationMillis:   next.Sub(w.FirstSeen[p]).Milliseconds(),
			PeakSlotLagBytes: w.PeakSlotLagByPhase[p],
		})
	}
}

// recordLoad replaces the results' account of the load with summary: what
// it did and how it failed, and how much of it committed after the binary
// exited. It is called with a snapshot at exit and again with the final
// summary, so each call starts from nothing.
func (r *results) recordLoad(s testutil.Summary, atExit testutil.Progress) {
	r.Load.Inserts = s.Inserts
	r.Load.Updates = s.Updates
	r.Load.Deletes = s.Deletes
	r.Load.Races = s.Races
	r.Load.MaxLatencyMillis = s.MaxLatency.Milliseconds()
	r.Load.CommittedAfterSwap = s.Committed() - atExit.Committed()
	r.Load.FailuresByCode = map[string]int{}
	for _, f := range s.Failures {
		r.Load.FailuresByCode[failureKey(f)]++
	}
}

// failureKey is the FailuresByCode key of a failure: its SQLSTATE, or
// deadlineCode for a client-side deadline, or otherCode for an error that
// is neither.
func failureKey(f testutil.Failure) string {
	if f.Code != "" {
		return f.Code
	}
	if errors.Is(f.Err, context.DeadlineExceeded) {
		return deadlineCode
	}
	return otherCode
}

// recordOutput keeps the binary's stdout: as the verdict when it is JSON,
// and as text when it is not, since a raw message that is not JSON cannot
// be encoded and would lose the whole record.
func (r *results) recordOutput(output []byte) {
	trimmed := bytes.TrimSpace(output)
	if json.Valid(trimmed) {
		r.Verdict = json.RawMessage(trimmed)
		return
	}
	r.Stdout = string(trimmed)
}

// write writes the results as indented JSON to path.
func (r *results) write(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode results: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write results to %s: %w", path, err)
	}
	return nil
}

// fileSHA256 returns the hex SHA-256 of the file at path.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("close %s: %v", path, err)
		}
	}()
	h := sha256.New()
	_, err = io.Copy(h, f)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}

// A binary that prints a usage error instead of a verdict still leaves a
// record: the text is kept as stdout, and the file is written and reads
// back with it.
func TestResultsKeepTheRecordOfARunWhoseStdoutIsNotJSON(t *testing.T) {
	res := newResults(config{Binary: "bin/pg-sprite", Rows: 10})
	res.recordOutput([]byte("pg-sprite: error: unknown flag --free-disk\n"))
	path := filepath.Join(t.TempDir(), "results.json")
	require.NoError(t, res.write(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var back map[string]any
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, "pg-sprite: error: unknown flag --free-disk", back["stdout"])
	assert.NotContains(t, back, "verdict")
}

// A verdict is kept as JSON, so a reader of the record queries its fields
// rather than parsing a string.
func TestResultsKeepAVerdictAsJSON(t *testing.T) {
	res := newResults(config{Binary: "bin/pg-sprite", Rows: 10})
	res.recordOutput([]byte(`{"outcome":"executed-via-copy-and-swap"}` + "\n"))
	path := filepath.Join(t.TempDir(), "results.json")
	require.NoError(t, res.write(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var back struct {
		Verdict map[string]string `json:"verdict"`
		Stdout  *string           `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, map[string]string{"outcome": "executed-via-copy-and-swap"}, back.Verdict)
	assert.Nil(t, back.Stdout)
}
