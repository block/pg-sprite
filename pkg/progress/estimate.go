package progress

import (
	"math"
	"time"
)

const (
	// runningPercentCeiling is the highest percent a running step reports.
	// A step's totals are estimates — the copy's row total is the catalog's
	// count at its start — so a counter can reach or pass its total while
	// work remains; the step ending, not the percent, is completion.
	runningPercentCeiling = 99.9
	// etaMinFraction is the share of a step that must be done before an
	// ETA is reported: below it the remaining time is the elapsed time
	// multiplied by more than the step's whole length again, and noise in
	// the first chunks or blocks dominates.
	etaMinFraction = 0.01
	// etaMinStepElapsed is how long a step must have run before an ETA is
	// reported, so the rate it is scaled from spans more than the step's
	// first few units of work.
	etaMinStepElapsed = 5 * time.Second
)

// estimate fills the running step's percent and ETA from the work counters
// observed in this poll and the step's elapsed time on the injected clock.
// Both stay nil — omitted from the JSON — when the step's operation has no
// measurable total or the total is not known yet, so a consumer can tell
// unknown from zero. Nothing is stored: every poll derives them afresh.
func estimate(s Snapshot) Snapshot {
	if s.Detail.Work == nil {
		return s
	}
	done, ok := stepCompletion(s.Detail)
	if !ok {
		return s
	}
	percent := min(math.Round(done.fraction*1000)/10, runningPercentCeiling)
	s.Detail.Percent = &percent
	if eta, ok := remaining(done, s.StepElapsed); ok {
		s.Detail.ETA = &eta
	}
	return s
}

// completion is how much of a step is done: the fraction, and whether a
// counter has already reached the total it is measured against, which
// proves that total was an underestimate.
type completion struct {
	fraction     float64
	totalReached bool
}

// stepCompletion measures the current step against its total. Only the
// copy, the checksum pass and a concurrent index build have one: the copy
// and the pass count rows against a catalog estimate, the build reports a
// phase and counters the server publishes. A catch-up runs until its owner
// cuts over and every other operation reports no work, so none of them has
// a fraction to give.
func stepCompletion(d Detail) (completion, bool) {
	switch d.Operation {
	case OperationCopy:
		return rowCompletion(d.Work.RowsCopied, d.Work.RowsTotal)
	case OperationChecksum:
		return rowCompletion(d.Work.RowsHashed, d.Work.RowsToHash)
	case OperationConcurrentIndex:
		fraction, ok := indexBuildFraction(d.ServerPhase, *d.Work)
		return completion{fraction: fraction}, ok
	default:
		return completion{}, false
	}
}

// rowCompletion is done rows over an estimated total, capped at the whole;
// with no total there is nothing to measure against.
func rowCompletion(done, total uint64) (completion, bool) {
	if total == 0 {
		return completion{}, false
	}
	return completion{fraction: min(ratio(done, total), 1), totalReached: done >= total}, true
}

// remaining projects the step's time left from its rate so far: the
// elapsed time scaled by the share left over the share done, to the
// nearest second and never below one while the step runs.
func remaining(done completion, stepElapsed time.Duration) (time.Duration, bool) {
	if !projectable(done, stepElapsed) {
		return 0, false
	}
	seconds := math.Round(stepElapsed.Seconds() * (1 - done.fraction) / done.fraction)
	return time.Duration(max(seconds, 1)) * time.Second, true
}

// projectable reports whether the step's rate so far says anything about
// the time it has left.
func projectable(done completion, stepElapsed time.Duration) bool {
	if done.totalReached {
		// The total is known to be short, so the work left is unknown.
		return false
	}
	if done.fraction < etaMinFraction {
		return false
	}
	return stepElapsed >= etaMinStepElapsed
}
