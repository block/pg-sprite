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
	// steps; the catch-up only fills the current step's counters. A tracker
	// polls one source at a time, so a catch-up that runs alongside the
	// copy is given no tracker, or one of its own: registering it on the
	// copier's tracker would hide the copier's counters, as registering the
	// copier on this one would hide the catch-up's.
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

// Status is one consistent snapshot of a catch-up as of the end of its last
// completed cycle: the stream's positions and what the buffer and the
// flushes have done with the changes, all recorded together.
type Status struct {
	// Delivered is the stream's delivered position: every transaction that
	// committed at or below it has been added to the buffer or discarded.
	Delivered decode.LSN
	// Confirmed is the position last reported to the server as applied,
	// which is where a restarted stream resumes from.
	Confirmed decode.LSN
	// WALEnd is the server's write position, pg_current_wal_lsn() read on
	// the pool after the last cycle's confirmation; zero until a cycle has
	// run. It is measured there rather than taken from the stream: a
	// walsender's keepalive carries its send position, how far it has
	// decoded, which stays small exactly while a backlog is undecoded.
	WALEnd decode.LSN
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

// Lag is how far the confirmed position trails the server's write position
// as the last cycle measured it, in WAL bytes; zero until a cycle has run,
// or once the confirmation has caught it.
func (s Status) Lag() uint64 {
	if s.WALEnd <= s.Confirmed {
		return 0
	}
	return uint64(s.WALEnd - s.Confirmed)
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
		status:    Status{Delivered: stream.Delivered(), Confirmed: stream.Confirmed()},
	}, nil
}

// Status returns the catch-up as of the end of its last completed cycle.
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
// so the slot releases WAL while nothing changes. The cycle ends by reading
// the server's write position on pool, so Status.Lag measures the WAL the
// catch-up has yet to work through, not how far the walsender has decoded.
//
// Run never returns nil: a catch-up has no end of its own, so its owner
// stops it by ending ctx once the shadow is close enough to cut over, and
// Run returns the cause ctx ended with, wrapped, once the stream reports
// it. A stopped catch-up leaves the stream failed and the slot at the last
// confirmed position, which is where the next stream resumes. A cycle
// error that is not the stop itself travels with the stop, so a caller
// checks the failure sentinels — ErrInvariantViolation, ErrTableLockLost,
// ErrBatchDeferred — before it reads the stop as clean. The whole run is
// bound to the table lock session, so losing the lock ends the cycle in
// flight, a flush or a quiet wait on the stream alike, and Run reports the
// loss as ErrTableLockLost under LK-1. A batch the shadow refuses once the
// copy has landed every key ends Run with ErrBatchDeferred: no chunk is
// left to change the outcome, so the schema change cannot converge. While
// it runs the catch-up is the tracker's work source (CatchupOptions.Tracker),
// and it has stopped being one by the time Run returns.
func (c *Catchup) Run(ctx context.Context, pool *pgxpool.Pool) error {
	if err := c.start(); err != nil {
		return err
	}
	// INV: LK-1 — moving the confirmed position is work the lock protects.
	ctx, unbind := c.flusher.lock.Bind(ctx)
	defer unbind()
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
// table lock outranks whatever the cancelled cycle reported, and a caller
// that ended ctx is told so.
func (c *Catchup) finish(ctx context.Context, err error) error {
	if lost := c.flusher.lockLost(); lost != nil {
		return lost
	}
	return stopped(ctx, c.flusher.target.Schema(), c.flusher.target.Table(), err)
}

// stopped reports a cycle error on an ended ctx as the caller's stop. A
// cycle error that is the stop itself — the stream's receive failing on
// the ended context — is folded into it; any other error raised in the
// same cycle travels with the stop, so a breach or a refusal that lands as
// the caller cancels is never read as a clean stop. On a live ctx the
// error is the cycle's own.
func stopped(ctx context.Context, schema, table string, err error) error {
	if ctx.Err() == nil {
		return err
	}
	stop := fmt.Errorf("catch-up on %s.%s stopped: %w", schema, table, context.Cause(ctx))
	if errors.Is(err, ctx.Err()) {
		return stop
	}
	return fmt.Errorf("%w; the cycle it stopped in failed: %w", stop, err)
}

// cycle is one gather, drain, flush, and confirm. Its status is recorded
// once, at the end, so a poll never pairs this cycle's counters with the
// last cycle's positions.
func (c *Catchup) cycle(ctx context.Context, pool *pgxpool.Pool) error {
	if err := c.gather(ctx); err != nil {
		return err
	}
	c.buffer.Release(c.stream.Delivered())
	// INV: CO-4 — the position is read after the last Add of this cycle.
	pos := c.positions.Position()
	batch := c.buffer.Drain(pos)
	result, err := c.flusher.Flush(ctx, pool, batch)
	outcome := cycleOutcome{batch: batch, result: result}
	switch {
	case errors.Is(err, ErrBatchDeferred) && copyLanded(pos):
		// INV: CO-6 — with every key landed and nothing in flight, the
		// shadow plus this batch is the source; a constraint that still
		// refuses it is one the source does not have.
		return fmt.Errorf("catch-up on %s.%s cannot converge, the copy has landed every key and no chunk is left to resolve the refusal: %w", c.flusher.target.Schema(), c.flusher.target.Table(), err)
	case errors.Is(err, ErrBatchDeferred):
		if err := c.buffer.Requeue(batch); err != nil {
			return err
		}
		outcome.deferred = true
	case err != nil:
		return err
	default:
		if err := c.buffer.Hold(result.Held); err != nil {
			return err
		}
	}
	// The confirmation reads the buffer after Requeue or Hold has put the
	// unapplied entries back; read before them, it would see an emptier
	// buffer and confirm past entries about to return to it (ST-4).
	if err := c.confirm(ctx); err != nil {
		return err
	}
	walEnd, err := c.measureWALEnd(ctx, pool)
	if err != nil {
		return err
	}
	c.record(outcome, walEnd)
	return nil
}

// copyLanded reports a copier position with every key landed and nothing
// in flight: the cut frontier is complete and no claimed chunk is unlanded.
func copyLanded(pos copier.Position) bool {
	return pos.Cut.Complete() && len(pos.InFlight) == 0
}

// cycleOutcome is what one cycle did with its drained batch, folded into
// the status once the cycle has confirmed.
type cycleOutcome struct {
	batch  Batch
	result Result
	// deferred is set when the flush refused the batch and it was requeued;
	// result is then empty.
	deferred bool
}

// gather adds changes to the buffer until MaxChanges have been added or
// Interval has passed since it started. A progress-only delivery counts
// towards neither; it moves the stream's positions, which the cycle's
// confirmation reads. Each change is buffered under the Delivered position
// it arrived with (Change.Delivered), the position the stream had reached
// before the change's own transaction: that is the position a confirmation
// may name while the change is unapplied, and what OldestPending returns.
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
	if bound <= c.stream.Confirmed() {
		return nil
	}
	return c.stream.Confirm(ctx, bound)
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

// measureWALEnd reads the server's write position on pool: the far end of
// the lag. It runs after the cycle's confirmation, so the position it
// returns is never below the one confirmed.
func (c *Catchup) measureWALEnd(ctx context.Context, pool *pgxpool.Pool) (decode.LSN, error) {
	var text string
	if err := pool.QueryRow(ctx, `SELECT pg_catalog.pg_current_wal_lsn()::text`).Scan(&text); err != nil {
		return 0, fmt.Errorf("catch-up on %s.%s: read the server's write position: %w", c.flusher.target.Schema(), c.flusher.target.Table(), err)
	}
	walEnd, err := decode.ParseLSN(text)
	if err != nil {
		return 0, fmt.Errorf("catch-up on %s.%s: read the server's write position: %w", c.flusher.target.Schema(), c.flusher.target.Table(), err)
	}
	return walEnd, nil
}

// record folds a completed cycle into the status under one lock: what the
// flush did with the batch, what the buffer holds, and the positions the
// cycle confirmed and measured. A requeued batch's entries are back in the
// buffer, waiting as the drain's deferred ones do, so they count as
// deferred; a committed flush's images and deletes count as applied.
func (c *Catchup) record(outcome cycleOutcome, walEnd decode.LSN) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Discarded += uint64(outcome.batch.Discarded)
	if outcome.deferred {
		c.status.Deferred = outcome.batch.Deferred + len(outcome.batch.Entries)
		c.status.BatchesDeferred++
	} else {
		c.status.Deferred = outcome.batch.Deferred
		c.status.Applied += uint64(outcome.result.Images + outcome.result.Deletes)
		if outcome.result.Fallback {
			c.status.Fallbacks++
		}
	}
	c.status.Buffered = c.buffer.Len()
	c.status.Held = c.buffer.Held()
	c.status.Delivered = c.stream.Delivered()
	c.status.Confirmed = c.stream.Confirmed()
	c.status.WALEnd = walEnd
}
