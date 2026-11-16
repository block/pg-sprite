package change

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// errVerificationRunning is returned by a verification started while another
// is armed: one runs at a time.
var errVerificationRunning = errors.New("change: a row verification is already running")

// rowParker holds the reader at the change a verification waits for. At most
// one watch is armed.
type rowParker struct {
	mu    sync.Mutex
	armed *parkedRow
}

// parkedRow is one verification: the watch, the change that fired it, and
// the two handoffs between the reader and the verifier.
type parkedRow struct {
	watch RowWatch
	// fired closes when the reader has buffered a matching change and is
	// held; change is set before it closes.
	fired  chan struct{}
	change rowChange
	// release closes when the verifier is done; the reader goes on.
	release     chan struct{}
	releaseOnce sync.Once
}

func (p *parkedRow) unpark() {
	p.releaseOnce.Do(func() { close(p.release) })
}

// arm installs a watch, refusing a second.
func (r *rowParker) arm(watch RowWatch) (*parkedRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.armed != nil {
		return nil, errVerificationRunning
	}
	p := &parkedRow{watch: watch, fired: make(chan struct{}), release: make(chan struct{})}
	r.armed = p
	return p, nil
}

// disarm removes the watch if it has not fired.
func (r *rowParker) disarm(p *parkedRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.armed == p {
		r.armed = nil
	}
}

// offer is called by the reader with each change it has just handed to the
// subscription. A change matching the armed watch fires it, and the reader
// waits until the verifier releases it or ctx ends.
func (r *rowParker) offer(ctx context.Context, tbl *TableInfo, c rowChange) {
	p := r.take(tbl, c)
	if p == nil {
		return
	}
	select {
	case <-p.release:
	case <-ctx.Done():
	}
}

// take fires and removes the armed watch when the change matches it.
func (r *rowParker) take(tbl *TableInfo, c rowChange) *parkedRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.armed
	if !watches(p, tbl, c) {
		return nil
	}
	r.armed = nil
	p.change = c
	close(p.fired)
	return p
}

// watches reports whether an armed watch names the change's row.
func watches(p *parkedRow, tbl *TableInfo, c rowChange) bool {
	if p == nil {
		return false
	}
	if p.watch.Schema != tbl.SchemaName || p.watch.Table != tbl.TableName {
		return false
	}
	return p.watch.Match(c.key)
}

// releaseAll releases a held reader and disarms the watch, for Close.
func (r *rowParker) releaseAll() {
	r.mu.Lock()
	p := r.armed
	r.armed = nil
	r.mu.Unlock()
	if p != nil {
		p.unpark()
	}
}

// VerifyRowAtNextChange waits, within WaitTimeout, for the next change to a
// row the watch matches, holds the reader there, flushes, and hands the
// change to verify. The reader handed the change over before it was held and
// reads nothing further until verify returns, so ErrRowRewritten is never
// returned: no later change to the row can be admitted in between.
func (s *LogicalSource) VerifyRowAtNextChange(ctx context.Context, watch RowWatch, verify RowVerifier) error {
	if watch.Match == nil {
		return errors.New("change: RowWatch.Match is required")
	}
	s.mu.Lock()
	done := s.readerDone
	s.mu.Unlock()
	if done == nil {
		return errNotStarted
	}
	p, err := s.parker.arm(watch)
	if err != nil {
		return err
	}
	defer p.unpark()
	defer s.parker.disarm(p)

	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.WaitTimeout)
	defer cancel()
	select {
	case <-p.fired:
	case <-done:
		if err := s.fatalErr(); err != nil {
			return err
		}
		return errReaderStopped
	case <-waitCtx.Done():
		return fmt.Errorf("wait for a change to %s.%s: %w", watch.Schema, watch.Table, waitCtx.Err())
	}
	if err := s.Flush(ctx); err != nil {
		return err
	}
	if s.GetDeltaLen() != 0 {
		return ErrFlushIncomplete
	}
	return verify(ctx, p.change.key, p.change.image, p.change.deleted)
}
