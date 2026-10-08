package applier

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/progress"
)

const (
	// DefaultCatchupInterval is how long a cycle gathers changes before it
	// drains and flushes what it holds when the stream is not busy enough
	// to fill it first.
	DefaultCatchupInterval = 250 * time.Millisecond
	// DefaultCatchupMaxChanges is how many changes a cycle adds before it
	// drains and flushes, whatever the interval says.
	DefaultCatchupMaxChanges = 10_000
)

var (
	// ErrInvalidCatchupOptions reports catch-up options that cannot bound a
	// cycle.
	ErrInvalidCatchupOptions = errors.New("invalid catch-up options")
	// ErrCatchupAlreadyRun reports a second Run on a Catchup. A catch-up is
	// single-use: its stream, once failed, is not reopened here.
	ErrCatchupAlreadyRun = errors.New("catch-up has already run")
)

// PositionSource reports where the copy stands, so a Drain can judge every
// buffered key against it (CO-4). A running or finished *copier.Copier is
// one; its Position stays valid after Run returns, with every key landed
// and nothing in flight.
type PositionSource interface {
	Position() copier.Position
}

// CatchupOptions bounds a cycle. Zero values take the defaults.
type CatchupOptions struct {
	// Interval is the longest a cycle gathers changes before it drains and
	// flushes. It is also the longest a quiet stream goes between
	// confirmations, so the slot's retained WAL is released at this pace.
	Interval time.Duration
	// MaxChanges is the most changes a cycle adds before it drains and
	// flushes: the bound on a flush transaction's size on a busy stream.
	MaxChanges int
	// Clock times each cycle's interval.
	Clock progress.Clock
	// Tracker, when set, is told the catch-up's work for the lifetime of
	// Run: the catch-up is its progress.WorkSource from before the first
	// cycle until just before Run returns. The caller owns the tracker's
	// steps; the catch-up only fills the current step's counters.
	Tracker *progress.Tracker
}

func (o CatchupOptions) withDefaults() CatchupOptions {
	if o.Interval == 0 {
		o.Interval = DefaultCatchupInterval
	}
	if o.MaxChanges == 0 {
		o.MaxChanges = DefaultCatchupMaxChanges
	}
	if o.Clock == nil {
		o.Clock = progress.WallClock{}
	}
	return o
}

// validate runs after withDefaults, so every field is set.
func (o CatchupOptions) validate() error {
	if o.Interval < time.Millisecond {
		return fmt.Errorf("%w: interval %s is below one millisecond; use zero for the default", ErrInvalidCatchupOptions, o.Interval)
	}
	if o.MaxChanges < 1 {
		return fmt.Errorf("%w: max changes %d is below one", ErrInvalidCatchupOptions, o.MaxChanges)
	}
	return nil
}

// Status is one consistent snapshot of a catch-up: the stream's positions
// and what the buffer and the flushes have done with the changes.
type Status struct {
	// Delivered is the stream's delivered position: every transaction that
	// committed at or below it has been added to the buffer or discarded.
	Delivered decode.LSN
	// Confirmed is the position last reported to the server as applied,
	// which is where a restarted stream resumes from.
	Confirmed decode.LSN
	// ServerWALEnd is the server's write position as it last reported it;
	// zero until it has.
	ServerWALEnd decode.LSN
	// Buffered is the number of keys the buffer holds after the last cycle:
	// entries deferred behind an in-flight chunk or held for a completion.
	Buffered int
	// Deferred is the number of entries the last drain left waiting for an
	// in-flight chunk, or for a batch a constraint refused (ErrBatchDeferred).
	Deferred int
	// Held is the number of images buffered with a completion pending.
	Held int
	// Applied is the number of images and delete markers committed flushes
	// have written to the shadow.
	Applied uint64
	// Discarded is the number of entries drains dropped because their keys
	// were uncut: the copier's own read sees those changes (CO-4).
	Discarded uint64
	// Fallbacks is the number of flushes that applied their batch whole-row
	// after a unique index or exclusion constraint refused the column-wise
	// writes (CO-6).
	Fallbacks uint64
	// BatchesDeferred is the number of batches a constraint refused even
	// whole-row, requeued for a later drain (CO-6).
	BatchesDeferred uint64
}

// Lag is how far the confirmed position trails the server's write position,
// in WAL bytes; zero until the server has reported a position, or once the
// confirmation has caught it.
func (s Status) Lag() uint64 {
	if s.ServerWALEnd <= s.Confirmed {
		return 0
	}
	return uint64(s.ServerWALEnd - s.Confirmed)
}

// Catchup owns the stream's consumer loop: it reads the stream into a
// Buffer, drains the buffer against the copier's Position, flushes each
// drained Batch through the Flusher, and confirms to the server what the
// shadow durably holds. One Catchup serves one stream for the lifetime of
// Run; the stream, the flusher, and the buffer are used from Run's goroutine
// alone, and Status and Work are safe to call from any other.
type Catchup struct {
	stream    *decode.Stream
	flusher   *Flusher
	positions PositionSource
	buffer    *Buffer
	opts      CatchupOptions

	mu     sync.Mutex
	status Status
	ran    bool
}

// NewCatchup prepares a catch-up that reads stream, judges keys against
// positions, and flushes through flusher. It refuses options that cannot
// bound a cycle.
func NewCatchup(stream *decode.Stream, flusher *Flusher, positions PositionSource, opts CatchupOptions) (*Catchup, error) {
	if stream == nil {
		return nil, fmt.Errorf("%w (ST-4): catch-up requires an open stream", ErrInvariantViolation)
	}
	if flusher == nil {
		return nil, fmt.Errorf("%w (ST-6): catch-up requires a flusher", ErrInvariantViolation)
	}
	if positions == nil {
		return nil, fmt.Errorf("%w (CO-4): catch-up requires the copier's position", ErrInvariantViolation)
	}
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &Catchup{
		stream:    stream,
		flusher:   flusher,
		positions: positions,
		buffer:    NewBuffer(),
		opts:      opts,
		status:    Status{Delivered: stream.Delivered(), Confirmed: stream.Confirmed(), ServerWALEnd: stream.ServerWALEnd()},
	}, nil
}

// Status returns the catch-up as of the end of its last cycle.
func (c *Catchup) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Run consumes the stream in cycles until ctx ends or the stream, the
// buffer, or a flush fails. Each cycle gathers changes until MaxChanges
// are buffered or Interval has passed, reads the copier's Position after
// the last Add — so a discarded uncut key's change is one the copier's
// read will see (CO-4) — drains, flushes the batch, puts a refused batch
// back (ErrBatchDeferred) or holds the images the flush completed but did
// not write, and then confirms the lowest of the stream's delivered
// position and the buffer's oldest pending position: everything below it
// is in the shadow or was discarded as uncut, so a stream reopened from
// it misses nothing (ST-4). An empty cycle on a quiet table still confirms,
// so the slot releases WAL while nothing changes.
//
// Run returns the cause ctx ended with, wrapped, once the stream reports
// it: a stopped catch-up leaves the stream failed and the slot at the last
// confirmed position, which is where the next stream resumes. Every flush
// runs under the lock session's Bind context, so losing the table lock
// cancels the flush in flight and Run reports the loss. While it runs the
// catch-up is the tracker's work source (CatchupOptions.Tracker), and it
// has stopped being one by the time Run returns.
func (c *Catchup) Run(ctx context.Context, pool *pgxpool.Pool) error {
	if err := c.start(); err != nil {
		return err
	}
	stopReporting := c.report()
	defer stopReporting()
	for {
		if err := c.cycle(ctx, pool); err != nil {
			return c.finish(ctx, err)
		}
	}
}

func (c *Catchup) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ran {
		return fmt.Errorf("%w: %s.%s", ErrCatchupAlreadyRun, c.flusher.target.Schema(), c.flusher.target.Table())
	}
	c.ran = true
	return nil
}

// finish turns the error that ended a cycle into Run's result: a lost
// table lock outranks whatever the cancelled flush reported, and a caller
// that ended ctx is told so rather than handed the stream's receive error.
func (c *Catchup) finish(ctx context.Context, err error) error {
	if lost := c.flusher.lockLost(); lost != nil {
		return lost
	}
	if ctx.Err() != nil {
		return fmt.Errorf("catch-up on %s.%s stopped: %w", c.flusher.target.Schema(), c.flusher.target.Table(), context.Cause(ctx))
	}
	return err
}

// cycle is one gather, drain, flush, and confirm.
func (c *Catchup) cycle(ctx context.Context, pool *pgxpool.Pool) error {
	if err := c.gather(ctx); err != nil {
		return err
	}
	c.buffer.Release(c.stream.Delivered())
	// INV: CO-4 — the position is read after the last Add of this cycle.
	batch := c.buffer.Drain(c.positions.Position())
	result, err := c.flusher.Flush(ctx, pool, batch)
	switch {
	case errors.Is(err, ErrBatchDeferred):
		if err := c.buffer.Requeue(batch); err != nil {
			return err
		}
		c.recordDeferral(batch)
	case err != nil:
		return err
	default:
		if err := c.buffer.Hold(result.Held); err != nil {
			return err
		}
		c.recordFlush(batch, result)
	}
	return c.confirm(ctx)
}

// gather adds changes to the buffer until MaxChanges have been added or
// Interval has passed since it started. A progress-only delivery counts
// towards neither; it moves the stream's positions, which the cycle's
// confirmation reads.
func (c *Catchup) gather(ctx context.Context) error {
	deadline := c.opts.Clock.Now().Add(c.opts.Interval)
	for added := 0; added < c.opts.MaxChanges; {
		remaining := deadline.Sub(c.opts.Clock.Now())
		if remaining <= 0 {
			return nil
		}
		delivery, err := c.stream.Next(ctx, remaining)
		if err != nil {
			return err
		}
		if delivery.Change == nil {
			continue
		}
		if err := c.buffer.Add(*delivery.Change); err != nil {
			return err
		}
		added++
	}
	return nil
}

// confirm reports to the server the highest position every unapplied change
// still lies above: the stream's delivered position, or the buffer's oldest
// pending position when that is lower. A position already confirmed is not
// sent again.
func (c *Catchup) confirm(ctx context.Context) error {
	// INV: ST-4
	oldest, hasPending := c.buffer.OldestPending()
	bound := confirmBound(c.stream.Delivered(), oldest, hasPending)
	if bound > c.stream.Confirmed() {
		if err := c.stream.Confirm(ctx, bound); err != nil {
			return err
		}
	}
	c.recordPositions()
	return nil
}

// confirmBound is the position a catch-up may confirm: delivered, lowered
// to the oldest pending position when the buffer holds one below it. The
// buffer's entries left it only through a committed flush or a discard, so
// everything the buffer no longer holds is accounted for below delivered.
func confirmBound(delivered decode.LSN, oldestPending decode.LSN, hasPending bool) decode.LSN {
	if hasPending && oldestPending < delivered {
		return oldestPending
	}
	return delivered
}

// recordFlush folds a committed flush into the status.
func (c *Catchup) recordFlush(batch Batch, result Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Applied += uint64(result.Images + result.Deletes)
	c.status.Discarded += uint64(batch.Discarded)
	c.status.Deferred = batch.Deferred
	if result.Fallback {
		c.status.Fallbacks++
	}
	c.recordBufferLocked()
}

// recordDeferral folds a refused and requeued batch into the status: its
// entries are back in the buffer, waiting as the drain's deferred ones do.
func (c *Catchup) recordDeferral(batch Batch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Discarded += uint64(batch.Discarded)
	c.status.Deferred = batch.Deferred + len(batch.Entries)
	c.status.BatchesDeferred++
	c.recordBufferLocked()
}

// recordPositions snapshots the stream's positions after a cycle's
// confirmation.
func (c *Catchup) recordPositions() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Delivered = c.stream.Delivered()
	c.status.Confirmed = c.stream.Confirmed()
	c.status.ServerWALEnd = c.stream.ServerWALEnd()
}

func (c *Catchup) recordBufferLocked() {
	c.status.Buffered = c.buffer.Len()
	c.status.Held = c.buffer.Held()
}
