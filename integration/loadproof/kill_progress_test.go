package loadproof_test

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/internal/testutil"
)

// progressFlag replaces --json on the runs the harness kills: a killed run
// prints no verdict, and with --progress=always on a stderr that is not a
// terminal the binary prints a plain progress line per change of step,
// operation, or ten-percent mark, which is how the harness learns the
// verify pass has compared a chunk.
const progressFlag = "--progress=always"

// killArgs is the proof's command line for a run the harness kills: the
// same change and flags as migrateArgs, with progress on stderr in place
// of the verdict JSON the run never prints.
func killArgs(c config, table testutil.WorkloadTable) []string {
	args := migrateArgs(c, table)
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			a = progressFlag
		}
		out = append(out, a)
	}
	return out
}

// checksumLabel is the operation field the binary's progress line shows
// while the verify pass runs.
const checksumLabel = "checksum"

// progressFieldSeparator is the separator between the fields of a plain
// progress line.
const progressFieldSeparator = "  "

// verifyPercent reads the verify pass's percent off one plain progress
// line. The line is human-facing prose, so the read is kept to two fields:
// a field naming the checksum operation and the percent field right after
// it. Any other line, a checksum line without a percent included, is not
// one.
func verifyPercent(line string) (float64, bool) {
	fields := strings.Split(line, progressFieldSeparator)
	for i, f := range fields {
		if f != checksumLabel || i+1 == len(fields) {
			continue
		}
		number, ok := strings.CutSuffix(fields[i+1], "%")
		if !ok {
			return 0, false
		}
		percent, err := strconv.ParseFloat(number, 64)
		if err != nil {
			return 0, false
		}
		return percent, true
	}
	return 0, false
}

// verifySeen is the last verify progress a killed run printed: the percent
// and the line it was read from, empty while no line reported one.
type verifySeen struct {
	Percent float64
	Line    string
}

// compared reports whether the line said the pass was past 0%: the
// percent counts rows from chunks whose comparison has ended, so above
// zero the pass has compared at least one chunk.
func (v verifySeen) compared() bool {
	return v.Line != "" && v.Percent > 0
}

// progressLines is a killed run's stderr. It keeps every byte written and
// reads each complete line as it arrives for the verify pass's percent,
// waking the kill poll when a line reports the pass past 0%.
type progressLines struct {
	mu      sync.Mutex
	all     bytes.Buffer
	partial []byte
	last    verifySeen
	// wake carries one pending wake-up for the kill poll; a full channel
	// already has one.
	wake chan struct{}
}

func newProgressLines() *progressLines {
	return &progressLines{wake: make(chan struct{}, 1)}
}

func (p *progressLines) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.all.Write(b)
	p.partial = append(p.partial, b...)
	for {
		end := bytes.IndexByte(p.partial, '\n')
		if end < 0 {
			break
		}
		line := string(p.partial[:end])
		p.partial = p.partial[end+1:]
		percent, ok := verifyPercent(line)
		if !ok {
			continue
		}
		p.last = verifySeen{Percent: percent, Line: line}
		if p.last.compared() {
			select {
			case p.wake <- struct{}{}:
			default:
			}
		}
	}
	return len(b), nil
}

// verify returns the last verify progress line read so far.
func (p *progressLines) verify() verifySeen {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *progressLines) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.all.String()
}

// The percent is read only off a line whose operation field is checksum,
// from the field right after it.
func TestVerifyPercentReadsTheChecksumLineOnly(t *testing.T) {
	cases := map[string]struct {
		line    string
		percent float64
		ok      bool
	}{
		"the checksum step":           {"step 4/6  checksum  42.50%  8.5K/20.0K rows  ETA 1s", 42.5, true},
		"the checksum step at 0%":     {"step 4/6  checksum  0.00%  0/20.0K rows", 0, true},
		"a single-step checksum line": {"checksum  10.00%  2.0K/20.0K rows", 10, true},
		"no row estimate, no percent": {"step 4/6  checksum  2.0K rows", 0, false},
		"the copy step":               {"step 2/6  copy  42.50%  8.5K/20.0K rows", 0, false},
		"the catch-up step":           {"step 3/6  catch-up  lag 1.2 MiB", 0, false},
		"an audit log line":           {"time=2026-01-01T00:00:00Z level=WARN msg=checksum", 0, false},
		"an empty line":               {"", 0, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			percent, ok := verifyPercent(tc.line)
			assert.Equal(t, tc.ok, ok)
			assert.InDelta(t, tc.percent, percent, 0)
		})
	}
}

// progressLines keeps every byte, reads lines split across writes, and
// wakes the poll only once a checksum line reports the pass past 0%.
func TestProgressLinesWakesOnAComparedChunk(t *testing.T) {
	p := newProgressLines()
	write := func(s string) {
		n, err := p.Write([]byte(s))
		assert.NoError(t, err)
		assert.Equal(t, len(s), n)
	}
	write("step 2/6  copy  50.00%  10.0K/20.0K rows\nstep 4/6  checksum  0.00%  0/20.0K rows\n")
	assert.Equal(t, verifySeen{Percent: 0, Line: "step 4/6  checksum  0.00%  0/20.0K rows"}, p.verify())
	assert.False(t, p.verify().compared(), "0% is a pass that has compared nothing yet")
	assert.Empty(t, p.wake)

	write("step 4/6  checksum  1")
	assert.Empty(t, p.wake, "half a line is not read")
	write("5.00%  3.0K/20.0K rows\n")
	assert.Equal(t, verifySeen{Percent: 15, Line: "step 4/6  checksum  15.00%  3.0K/20.0K rows"}, p.verify())
	assert.True(t, p.verify().compared())
	assert.Len(t, p.wake, 1)
	assert.Equal(t, "step 2/6  copy  50.00%  10.0K/20.0K rows\nstep 4/6  checksum  0.00%  0/20.0K rows\nstep 4/6  checksum  15.00%  3.0K/20.0K rows\n", p.String())
}
