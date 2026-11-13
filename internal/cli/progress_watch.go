package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/progress"
)

// defaultProgressInterval is how often a running change's progress is
// polled and its line redrawn.
const defaultProgressInterval = time.Second

// ansiClearLine returns the cursor to the start of the line and erases it,
// so the next progress line replaces the last one in place.
const ansiClearLine = "\r\x1b[K"

// plainPercentBucket is the percent step at which the plain form prints a
// new line for a step whose other fields have not changed.
const plainPercentBucket = 10

// progressForm decides whether migrate shows its progress line and in which
// form. Machine output (--json) and a dry run never show it. always shows
// it on any stderr, auto only on a terminal that can redraw a line; it is
// redrawn in place on such a terminal and printed as plain lines anywhere
// else.
func progressForm(mode string, machineOutput, terminal bool, term string) (show, redraw bool) {
	if machineOutput {
		return false, false
	}
	redraws := terminal && term != "dumb"
	switch mode {
	case "never":
		return false, false
	case "always":
		return true, redraws
	default:
		return redraws, redraws
	}
}

// startProgress attaches a progress tracker to opts and starts the
// goroutine that polls it to stderr while the change runs, when this run
// shows progress at all. The returned stop ends the poller, waits for it to
// return, and clears or finishes its line; the caller calls it once the
// engine call returns and before the verdict prints, so the verdict never
// shares a line with progress. Without progress, opts is returned unchanged
// and stop does nothing.
func (c *MigrateCmd) startProgress(ctx context.Context, opts migrate.Options) (migrate.Options, func(), error) {
	out := c.diagWriter()
	show, redraw := progressForm(c.Progress, c.JSON || c.DryRun, isTerminal(out), os.Getenv("TERM"))
	if !show {
		return opts, func() {}, nil
	}
	tracker, err := progress.NewTracker(progress.WallClock{})
	if err != nil {
		return opts, nil, fmt.Errorf("progress tracker: %w", err)
	}
	opts.Progress = tracker
	interval := c.progressInterval
	if interval == 0 {
		interval = defaultProgressInterval
	}
	ticker := time.NewTicker(interval)
	display := &progressDisplay{out: out, redraw: redraw}
	pollCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { pollProgress(pollCtx, tracker, ticker.C, display) })
	return opts, func() {
		cancel()
		wg.Wait()
		ticker.Stop()
		display.finish()
	}, nil
}

// pollProgress shows the tracker's snapshot on every tick until ctx ends
// or a write to the display fails. A tick's read waits for the tracker's
// poll gate under ctx, so ending ctx stops a waiting poll at once; a read
// already issued is bounded by the tracker itself.
func pollProgress(ctx context.Context, tracker *progress.Tracker, ticks <-chan time.Time, display *progressDisplay) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		// A read that fails still returns the last-known snapshot, which is
		// the state worth showing; the next tick reads again.
		s, _ := tracker.Progress(ctx)
		if ctx.Err() != nil {
			return
		}
		if err := display.show(s); err != nil {
			return
		}
	}
}

// progressDisplay writes progress lines in one of two forms: redrawn in
// place on a terminal, or one plain line per change of step, operation,
// server phase, or percent bucket, so a log keeps a readable history. Only
// the poller goroutine calls show; finish runs after it has returned.
type progressDisplay struct {
	out    io.Writer
	redraw bool
	// drawn records that a redrawn line is on screen and must be cleared.
	drawn bool
	// last is the plain form's last printed key; printed says one exists.
	last    progressKey
	printed bool
}

// progressKey is what the plain form compares to decide whether a snapshot
// is worth a new line: an ETA or counter tick alone is not.
type progressKey struct {
	step          int
	operation     progress.Operation
	serverPhase   string
	percentBucket int
}

func keyOf(s progress.Snapshot) progressKey {
	bucket := -1
	if s.Detail.Percent != nil {
		bucket = int(math.Floor(*s.Detail.Percent / plainPercentBucket))
	}
	return progressKey{step: s.Step, operation: s.Detail.Operation, serverPhase: s.Detail.ServerPhase, percentBucket: bucket}
}

func (p *progressDisplay) show(s progress.Snapshot) error {
	line := renderProgress(s)
	if line == "" {
		return nil
	}
	if p.redraw {
		p.drawn = true
		_, err := fmt.Fprint(p.out, ansiClearLine+line)
		return err
	}
	key := keyOf(s)
	if p.printed && key == p.last {
		return nil
	}
	p.last, p.printed = key, true
	_, err := fmt.Fprintln(p.out, line)
	return err
}

// finish clears a redrawn line so the verdict and any later diagnostics
// start on a clean line; plain lines already end in a newline.
func (p *progressDisplay) finish() {
	if p.redraw && p.drawn {
		// Nowhere left to report a failed stderr write.
		_, _ = fmt.Fprint(p.out, ansiClearLine)
	}
}
