package change

import (
	"context"
	"fmt"

	"github.com/block/pg-sprite/pkg/decode"
)

// read is the reader goroutine: it owns the stream from open until it
// returns. Each pass confirms the flushed position, reads one delivery,
// hands a change to the subscription, and records the delivered position.
// It returns when ctx ends or the stream fails; a failure is recorded as the
// source's fatal error.
func (s *LogicalSource) read(ctx context.Context, stream *decode.Stream) {
	defer s.closeStream(ctx, stream)
	for ctx.Err() == nil {
		if err := s.confirm(ctx, stream); err != nil {
			s.fail(ctx, err)
			return
		}
		delivery, err := stream.Next(ctx, s.cfg.ReadWait)
		if err != nil {
			s.fail(ctx, err)
			return
		}
		if delivery.Change != nil {
			if err := s.deliver(ctx, *delivery.Change); err != nil {
				s.fail(ctx, err)
				return
			}
		}
		s.advance(delivery.Delivered)
	}
}

// confirm reports the flushed position to the slot when it has moved past
// what the stream last confirmed.
func (s *LogicalSource) confirm(ctx context.Context, stream *decode.Stream) error {
	s.mu.Lock()
	flushed := s.flushed
	s.mu.Unlock()
	if flushed <= stream.Confirmed() {
		return nil
	}
	// INV: ST-4 — the slot is confirmed only to a position every change
	// below which the subscription has flushed.
	if err := stream.Confirm(ctx, flushed); err != nil {
		return fmt.Errorf("confirm %s to slot %s: %w", flushed, stream.Slot(), err)
	}
	return nil
}

// deliver hands one change to the subscription, unless Stop has ended
// delivery. A change matching an armed verification holds the reader once
// it is buffered.
func (s *LogicalSource) deliver(ctx context.Context, ev decode.ChangeEvent) error {
	s.received.Add(1)
	if s.stopped.Load() {
		s.dispatched.Add(1)
		return nil
	}
	changes, err := rowChanges(ev, s.table, s.immutableKey)
	if err != nil {
		return err
	}
	for _, c := range changes {
		s.sub.HasChanged(c.key, c.image, c.deleted)
		s.parker.offer(ctx, s.table, c)
	}
	s.dispatched.Add(1)
	return nil
}

// advance records that every change below lsn has been handed to the
// subscription, and wakes the waiters on it.
func (s *LogicalSource) advance(lsn decode.LSN) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lsn <= s.buffered {
		return
	}
	s.buffered = lsn
	close(s.advanced)
	s.advanced = make(chan struct{})
}

// fail records err as the source's fatal error, unless the reader is
// stopping because its context ended, and reports it to OnFatal once.
func (s *LogicalSource) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	first := s.fatal == nil
	if first {
		s.fatal = fmt.Errorf("change source for %s.%s: %w", s.cfg.Target.Schema(), s.cfg.Target.Table(), err)
	}
	fatal := s.fatal
	s.mu.Unlock()
	if first && s.cfg.OnFatal != nil {
		s.cfg.OnFatal(fatal)
	}
}

// fatalErr is the error that ended the stream, or nil.
func (s *LogicalSource) fatalErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal
}

// closeStream closes the replication connection within CloseTimeout. The
// close error is logged: the stream is already done with.
func (s *LogicalSource) closeStream(ctx context.Context, stream *decode.Stream) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.CloseTimeout)
	defer cancel()
	if err := stream.Close(closeCtx); err != nil {
		s.cfg.Logger.Warn("close the change stream",
			"schema", s.cfg.Target.Schema(), "table", s.cfg.Target.Table(), "slot", stream.Slot(), "error", err)
	}
}
