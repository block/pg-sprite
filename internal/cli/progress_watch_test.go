package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// The progress line never shows beside machine output, auto shows it only
// on a terminal that can redraw it, and always shows it anywhere: redrawn
// on such a terminal, plain lines elsewhere.
func TestProgressForm(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          string
		machineOutput bool
		terminal      bool
		term          string
		show, redraw  bool
	}{
		{name: "auto on a terminal redraws", mode: "auto", terminal: true, term: "xterm-256color", show: true, redraw: true},
		{name: "auto off a terminal hides", mode: "auto", terminal: false, term: "xterm-256color"},
		{name: "auto on a dumb terminal hides", mode: "auto", terminal: true, term: "dumb"},
		{name: "unset behaves as auto", mode: "", terminal: true, term: "xterm", show: true, redraw: true},
		{name: "auto with json hides on a terminal", mode: "auto", machineOutput: true, terminal: true, term: "xterm"},
		{name: "always off a terminal prints plain lines", mode: "always", terminal: false, term: "xterm", show: true},
		{name: "always on a dumb terminal prints plain lines", mode: "always", terminal: true, term: "dumb", show: true},
		{name: "always on a terminal redraws", mode: "always", terminal: true, term: "xterm", show: true, redraw: true},
		{name: "always with machine output hides", mode: "always", machineOutput: true, terminal: true, term: "xterm"},
		{name: "never hides on a terminal", mode: "never", terminal: true, term: "xterm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			show, redraw := progressForm(tc.mode, tc.machineOutput, tc.terminal, tc.term)
			assert.Equal(t, tc.show, show, "show")
			assert.Equal(t, tc.redraw, redraw, "redraw")
		})
	}
}

// --progress=always asks for a line the machine-readable and dry-run forms
// never show, so the grammar rejects the pair instead of ignoring it.
func TestProgressAlwaysRejectedWithJSONAndDryRun(t *testing.T) {
	parse := func(args ...string) error {
		c := New("test")
		k, err := kong.New(c, kong.Vars{"version": "test"})
		require.NoError(t, err)
		_, err = k.Parse(append([]string{
			"migrate",
			"--url", "postgres://user@localhost:5432/app",
			"--alter", "ALTER TABLE t ADD COLUMN c int",
		}, args...))
		return err
	}
	err := parse("--progress=always", "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--progress=always cannot be combined with --json")
	err = parse("--progress=always", "--dry-run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--progress=always cannot be combined with --dry-run")
	require.NoError(t, parse("--json"), "auto, the default, is simply off with --json")
	require.NoError(t, parse("--progress=never", "--json"))
}

// The plain form prints a line when the step, operation, server phase, or
// ten-percent mark changes, and not for an ETA or counter tick alone.
func TestProgressDisplayPlainPrintsOnlyChanges(t *testing.T) {
	var out strings.Builder
	d := &progressDisplay{out: &out}
	copyAt := func(p float64, rows uint64) progress.Snapshot {
		return running(1, 2, progress.Detail{Operation: progress.OperationCopy, Active: true, Work: &progress.Work{RowsCopied: rows, RowsTotal: 1000}, Percent: new(p)})
	}
	for _, s := range []progress.Snapshot{
		running(0, 2, progress.Detail{Operation: progress.OperationAdmitting, Active: true}),
		running(0, 2, progress.Detail{Operation: progress.OperationAdmitting, Active: true}),
		copyAt(1, 10),
		copyAt(9.9, 99),
		copyAt(10, 100),
		copyAt(12, 120),
		running(2, 2, progress.Detail{Operation: progress.OperationValidate, Active: true}),
		{Phase: progress.PhaseFinished, Step: 2, TotalSteps: 2},
	} {
		require.NoError(t, d.show(s))
	}
	d.finish()
	assert.Equal(t, strings.Join([]string{
		"step 0/2  admitting",
		"step 1/2  copy  1.0%  10/1.0K rows",
		"step 1/2  copy  10.0%  100/1.0K rows",
		"step 2/2  validate constraint",
	}, "\n")+"\n", out.String())
}

// On a terminal every poll replaces the line in place, and finish clears
// it so the verdict starts on a clean line.
func TestProgressDisplayRedrawReplacesAndClears(t *testing.T) {
	var out strings.Builder
	d := &progressDisplay{out: &out, redraw: true}
	require.NoError(t, d.show(running(1, 1, progress.Detail{Operation: progress.OperationCatchUp, Active: true, Work: &progress.Work{LagBytes: 2048}})))
	require.NoError(t, d.show(running(1, 1, progress.Detail{Operation: progress.OperationCatchUp, Active: true, Work: &progress.Work{LagBytes: 1024}})))
	d.finish()
	assert.Equal(t, "\r\x1b[Kcatch-up  lag 2.0 KiB\r\x1b[Kcatch-up  lag 1.0 KiB\r\x1b[K", out.String())
}

// A redrawn display that never drew leaves stderr untouched.
func TestProgressDisplayRedrawFinishWithoutLineWritesNothing(t *testing.T) {
	var out strings.Builder
	d := &progressDisplay{out: &out, redraw: true}
	d.finish()
	assert.Empty(t, out.String())
}

// The poller reads the tracker on each injected tick and returns once its
// context ends, so stop can wait for it.
func TestPollProgressShowsEachTickAndStopsOnCancel(t *testing.T) {
	tracker, err := progress.NewTracker(fixedClock{t: time.Unix(0, 0)})
	require.NoError(t, err)
	tracker.Start(2, progress.OperationAdmitting)

	var out syncWriter
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { pollProgress(ctx, tracker, ticks, &progressDisplay{out: &out}) })

	// An unbuffered send returns once the poller took the tick, so the
	// send after it returns only once that tick's line is written. The
	// tick read around StartStep may see either step; the plain form's
	// dedupe makes the output the same both ways.
	ticks <- time.Time{}
	ticks <- time.Time{}
	tracker.StartStep(1, progress.OperationOptimistic, "ALTER TABLE public.t ADD COLUMN c int")
	ticks <- time.Time{}
	ticks <- time.Time{}
	cancel()
	wg.Wait()
	assert.Equal(t, "step 0/2  admitting\nstep 1/2  statement\n", out.String())
}
