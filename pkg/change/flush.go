package change

import (
	"context"
	"errors"
	"fmt"

	"github.com/block/pg-sprite/pkg/decode"
)

// Flush flushes the subscription. When the subscription reports everything
// flushed, the position read before the flush becomes the flushed position,
// and the reader confirms it to the slot.
func (s *LogicalSource) Flush(ctx context.Context) error {
	return s.flush(ctx, false, nil)
}

// FlushUnderTableLock flushes under the caller's target locks, waits for the
// stream to deliver everything the source committed so far, and flushes
// again, so the changes the first flush raced are applied under the locks
// too.
func (s *LogicalSource) FlushUnderTableLock(ctx context.Context, locks []TableLock) error {
	if len(locks) == 0 {
		return errors.New("change: FlushUnderTableLock requires at least one table lock")
	}
	if err := s.flush(ctx, true, locks); err != nil {
		return err
	}
	if err := s.BlockWait(ctx); err != nil {
		return err
	}
	return s.flush(ctx, true, locks)
}

// flush runs one flush of the subscription. The buffered position is read
// before the subscription flushes: every change below it was handed over
// before it was recorded, so a flush that leaves nothing behind has flushed
// all of them.
func (s *LogicalSource) flush(ctx context.Context, underLock bool, locks []TableLock) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if err := s.fatalErr(); err != nil {
		return err
	}
	s.mu.Lock()
	sub, buffered := s.sub, s.buffered
	s.mu.Unlock()
	if sub == nil {
		return nil
	}
	start, rows := s.cfg.Clock.Now(), sub.Length()
	complete, err := sub.Flush(ctx, underLock, locks)
	if err != nil {
		return fmt.Errorf("flush %s.%s: %w", s.table.SchemaName, s.table.TableName, err)
	}
	residual := sub.Length()
	s.mu.Lock()
	defer s.mu.Unlock()
	if complete && buffered > s.flushed {
		s.flushed = buffered
	}
	s.flushResidual = residual
	s.flushCount++
	s.lastFlushAt = s.cfg.Clock.Now()
	s.lastFlushDuration = s.lastFlushAt.Sub(start)
	s.lastFlushRows = rows
	return nil
}

// BlockWait writes a marker into the WAL and waits, within WaitTimeout, for
// the stream to deliver past it: every transaction committed before the
// marker has then been handed to the subscription.
func (s *LogicalSource) BlockWait(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.WaitTimeout)
	defer cancel()
	marker, err := s.markWAL(ctx)
	if err != nil {
		return err
	}
	return s.awaitBuffered(ctx, marker)
}

// markWAL writes a WAL marker in a transaction of its own and returns the
// position past it.
func (s *LogicalSource) markWAL(ctx context.Context) (decode.LSN, error) {
	tx, err := s.cfg.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("mark the WAL position: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	marker, err := decode.MarkWAL(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("mark the WAL position: %w", err)
	}
	return marker, nil
}

// awaitBuffered waits until the buffered position reaches target, the
// stream fails, the reader stops, or ctx ends.
func (s *LogicalSource) awaitBuffered(ctx context.Context, target decode.LSN) error {
	for {
		s.mu.Lock()
		buffered, advanced, done, fatal := s.buffered, s.advanced, s.readerDone, s.fatal
		s.mu.Unlock()
		switch {
		case fatal != nil:
			return fatal
		case done == nil:
			return errNotStarted
		case buffered >= target:
			return nil
		}
		select {
		case <-advanced:
		case <-done:
			if err := s.fatalErr(); err != nil {
				return err
			}
			return errReaderStopped
		case <-ctx.Done():
			return fmt.Errorf("wait for the stream to deliver %s, at %s: %w", target, buffered, ctx.Err())
		}
	}
}
