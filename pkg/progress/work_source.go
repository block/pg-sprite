package progress

import "context"

// WorkSource reports engine-derived work for the current step: the counters
// of an operation whose progress PostgreSQL publishes no view for, such as
// the copy-and-swap row copy, which knows its own rows copied from the
// chunks it has landed. The tracker polls it inside Progress, on the
// observer's context, so the source must be safe to call from any goroutine
// while the step runs. Every counter the source does not measure stays zero
// — a source reports what it observed, never an estimate dressed as a count.
type WorkSource interface {
	Work(ctx context.Context) (Work, error)
}

// SetWorkSource makes source the current step's work for every later poll.
// A step's work comes from one place: setting a source drops any concurrent
// build the step was polling, and SetConcurrentBuild drops the source. The
// engine calls StopWorkSource before the source's state goes away.
func (t *Tracker) SetWorkSource(source WorkSource) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = source
	t.session, t.buildPID = nil, 0
}

// StopWorkSource waits for an in-flight observation and then stops polling
// the source. It is the fence between the source and its owner's return:
// the engine calls it before the state the source reads is released, so
// no poll that began while the step ran completes against a source whose
// owner has gone.
func (t *Tracker) StopWorkSource() {
	t.pollMu.Lock()
	defer t.pollMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = nil
}

// observeEngineWork merges one source observation into s. On a source error
// the snapshot still carries the last-known tracker state, as the concurrent
// build path does on a query error.
func observeEngineWork(ctx context.Context, s Snapshot, source WorkSource) (Snapshot, error) {
	work, err := source.Work(ctx)
	if err != nil {
		return s, err
	}
	s.Detail.Work = &work
	return s, nil
}
