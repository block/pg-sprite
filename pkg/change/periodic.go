package change

import (
	"context"
	"sync"
	"time"
)

// periodicFlush is the state of a running periodic flush.
type periodicFlush struct {
	mu     sync.Mutex
	stop   context.CancelFunc
	done   chan struct{}
	runner sync.WaitGroup
}

// StartPeriodicFlush flushes every interval until StopPeriodicFlush, Close,
// or the end of ctx. A flush that fails ends the stream: the error is the
// source's fatal error, returned by every later Flush and BlockWait and
// handed to OnFatal. A second call while one runs, a call after Close, and a
// non-positive interval do nothing.
func (s *LogicalSource) StartPeriodicFlush(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	p := &s.periodic
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stop != nil {
		return
	}
	flushCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	p.stop, p.done = stop, done
	p.runner.Go(func() {
		defer close(done)
		s.flushEvery(flushCtx, interval)
	})
}

// flushEvery flushes at each tick until ctx ends or a flush fails.
func (s *LogicalSource) flushEvery(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := s.Flush(ctx); err != nil {
			s.fail(ctx, err)
			return
		}
	}
}

// StopPeriodicFlush stops the periodic flush and waits for it to return.
func (s *LogicalSource) StopPeriodicFlush() {
	p := &s.periodic
	p.mu.Lock()
	stop := p.stop
	p.stop, p.done = nil, nil
	p.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	p.runner.Wait()
}
