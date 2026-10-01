package progress

import "context"

// WorkSource reports engine-derived work for the current step: the counters
// of an operation whose progress PostgreSQL publishes no view for, such as
// the copy-and-swap row copy, which knows its own rows copied from the
// chunks it has landed, or the checksum pass, which knows the chunks it has
// compared and repaired. The tracker polls it inside Progress, on the
// observer's context, so the source must be safe to call from any goroutine
// while the step runs; the tracker itself never calls it from more than one
// goroutine at a time. Every counter the source does not measure stays zero
// — a source reports what it observed, never an estimate dressed as a count.
//
// Work is on the engine's own stop path, so it has three obligations beyond
// returning counters:
//
//   - Work bounds itself. SetWorkSource, SetConcurrentBuild and
//     StopWorkSource wait for a poll in flight, and an observer may hand
//     the tracker a context that never ends, so a Work that only returns
//     when its caller gives up can stall the engine indefinitely. Work
//     reads memory, or reads the catalog on a session whose
//     statement_timeout is set, and honours ctx as well — a cancelled
//     observer must not keep the poll alive.
//   - Work takes no lock the engine holds while it calls SetWorkSource,
//     SetConcurrentBuild or StopWorkSource. Those calls wait for Work to
//     return; a Work that waits for the engine's lock would deadlock the
//     step with its observer.
//   - A Work that reads the catalog is a CO-9 read site: every catalog
//     relation, function and operator is pg_catalog-qualified, and its test
//     runs under a search_path that shadows the names it uses.
type WorkSource interface {
	Work(ctx context.Context) (Work, error)
}

// SetWorkSource makes source the current step's work for every later poll.
// A step's work comes from one place: setting a source drops any concurrent
// build the step was polling, and SetConcurrentBuild drops the source.
//
// Like StopWorkSource, it waits for an observation or cancel signal in
// flight, so the build it replaces is never released while a poll still
// reads its session, and a source it replaces has left its last poll
// before the engine lets the state behind it go. The engine calls
// StopWorkSource before the source's state goes away.
func (t *Tracker) SetWorkSource(source WorkSource) {
	t.pollMu.Lock()
	defer t.pollMu.Unlock()
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
