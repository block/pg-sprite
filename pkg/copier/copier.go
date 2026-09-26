package copier

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
)

// DefaultWorkers is the number of chunks copied concurrently when Options
// leaves Workers zero.
const DefaultWorkers = 4

var (
	// ErrInvalidOptions reports copier options that cannot bound the copy: a
	// negative worker count, or a timeout below PostgreSQL's one-millisecond
	// resolution, which the server would read as no timeout at all.
	ErrInvalidOptions = errors.New("invalid copier options")
	// ErrAlreadyRun reports a second Run on the same Copier; a Copier drives
	// one copy and is then only a record of where it stopped.
	ErrAlreadyRun = errors.New("copier has already run")
)

// Options bounds the copy. Zero values take the defaults: DefaultWorkers
// workers, the dbconn session timeouts for every chunk transaction, the
// ChunkerOptions defaults, and the wall clock.
type Options struct {
	// Workers is the number of chunk transactions in flight at once. The
	// pool's connection limit bounds it from above.
	Workers int
	// LockTimeout bounds every lock wait inside a chunk transaction.
	LockTimeout time.Duration
	// StatementTimeout bounds every statement inside a chunk transaction.
	StatementTimeout time.Duration
	// Chunker sizes the chunks.
	Chunker ChunkerOptions
	// Clock times each chunk for the chunker's feedback (D12).
	Clock progress.Clock
}

func (o Options) withDefaults() Options {
	if o.Workers == 0 {
		o.Workers = DefaultWorkers
	}
	if o.LockTimeout == 0 {
		o.LockTimeout = dbconn.DefaultLockTimeout
	}
	if o.StatementTimeout == 0 {
		o.StatementTimeout = dbconn.DefaultStatementTimeout
	}
	if o.Clock == nil {
		o.Clock = progress.WallClock{}
	}
	return o
}

// validate runs after withDefaults, so every field is set.
func (o Options) validate() error {
	if o.Workers < 1 {
		return fmt.Errorf("%w: workers %d is below one", ErrInvalidOptions, o.Workers)
	}
	for _, timeout := range []struct {
		name  string
		value time.Duration
	}{{"lock timeout", o.LockTimeout}, {"statement timeout", o.StatementTimeout}} {
		if timeout.value < time.Millisecond {
			// INV: LK-2
			return fmt.Errorf("%w: %s %s is below PostgreSQL's one-millisecond resolution; use zero for the default", ErrInvalidOptions, timeout.name, timeout.value)
		}
	}
	return nil
}

// Copier copies a proven source table into its built shadow, chunk by chunk,
// with several workers, never overwriting a shadow row (CO-4). It runs only
// under the table's lock session and reports its progress as a Position the
// applier can judge captured changes against.
type Copier struct {
	target  preflight.CopySwapTarget
	shadow  Shadow
	lock    *dbconn.TableLockSession
	chunker *Chunker
	opts    Options
	// sql is the one insert statement every chunk runs, frozen at
	// construction so no worker builds SQL.
	sql string

	mu     sync.Mutex
	ledger *ledger
	ran    bool
}

// NewCopier prepares a copy of target into shadow that resumes after from
// (or covers the whole key space when from is the zero watermark). It refuses
// a proof, shadow, or lock session that does not describe this table, and
// options that cannot bound the copy.
func NewCopier(target preflight.CopySwapTarget, shadow Shadow, lock *dbconn.TableLockSession, from Watermark, opts Options) (*Copier, error) {
	// INV: ST-6
	if target.Table() == "" {
		return nil, fmt.Errorf("%w (ST-6): copy-and-swap target proof is empty", ErrInvariantViolation)
	}
	if err := checkShadow(target, shadow); err != nil {
		return nil, err
	}
	if err := requireTableLock(lock, target); err != nil {
		return nil, err
	}
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	chunker, err := NewChunker(target, from, opts.Chunker)
	if err != nil {
		return nil, err
	}
	return &Copier{
		target:  target,
		shadow:  shadow,
		lock:    lock,
		chunker: chunker,
		opts:    opts,
		sql:     copySQL(target, shadow),
		ledger:  newLedger(from),
	}, nil
}

// requireTableLock refuses to copy without the per-table lock that keeps a
// second engine instance off the same table: the session must exist, carry
// a populated proof for the proven table, and not have reported loss.
func requireTableLock(lock *dbconn.TableLockSession, target preflight.CopySwapTarget) error {
	// INV: LK-1
	if lock == nil {
		return fmt.Errorf("%w (LK-1): copy requires a table lock session", ErrInvariantViolation)
	}
	held := lock.Lock()
	if held.Table() == "" {
		return fmt.Errorf("%w (LK-1): table lock proof is empty", ErrInvariantViolation)
	}
	if held.Schema() != target.Schema() || held.Table() != target.Table() {
		return fmt.Errorf("%w (LK-1): table lock is for %s.%s, proof is for %s.%s", ErrInvariantViolation, held.Schema(), held.Table(), target.Schema(), target.Table())
	}
	if err := lock.Err(); err != nil {
		return fmt.Errorf("%w (LK-1): table lock was lost before the copy: %w", ErrInvariantViolation, err)
	}
	return nil
}

// Position snapshots the copier's progress under one lock acquisition. It is
// safe to call from any goroutine while Run is in progress.
func (c *Copier) Position() Position {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ledger.position()
}

// Run copies every chunk after the resume watermark and returns once the
// whole key space has landed, or once the first failure or cancellation has
// stopped every worker. It returns only after every worker has exited, so no
// chunk transaction is in flight when it does (LK-3); a caller that then
// checkpoints Position().Watermark records only committed work. Every
// chunk transaction runs under the lock session's Bind context, so losing
// the table lock cancels the statements in flight and Run reports the loss.
func (c *Copier) Run(ctx context.Context, pool *pgxpool.Pool) error {
	if err := c.start(); err != nil {
		return err
	}
	ctx, unbind := c.lock.Bind(ctx)
	defer unbind()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	var wg sync.WaitGroup
	for range c.opts.Workers {
		wg.Go(func() {
			if err := c.work(ctx, pool); err != nil {
				stop(err)
			}
		})
	}
	wg.Wait()
	// INV: LK-3
	return c.finish(context.Cause(ctx))
}

func (c *Copier) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ran {
		return fmt.Errorf("%w: %s.%s", ErrAlreadyRun, c.target.Schema(), c.target.Table())
	}
	c.ran = true
	return nil
}

// finish turns the reason the workers stopped into Run's result. A lost
// table lock outranks whatever statement error the cancelled context
// produced; a clean stop must have covered the key space with nothing left
// in flight.
func (c *Copier) finish(cause error) error {
	if lost := c.lock.Err(); lost != nil {
		// INV: LK-1
		return fmt.Errorf("%w (LK-1): table lock was lost during the copy: %w", ErrInvariantViolation, lost)
	}
	if cause != nil {
		return cause
	}
	pos := c.Position()
	// INV: LK-3
	if len(pos.InFlight) != 0 {
		return fmt.Errorf("%w (LK-3): copy of %s.%s stopped with %d chunks in flight", ErrInvariantViolation, c.target.Schema(), c.target.Table(), len(pos.InFlight))
	}
	// INV: CO-4
	if !pos.Watermark.Valid() || pos.Watermark.Value() != math.MaxInt64 {
		return fmt.Errorf("%w (CO-4): copy of %s.%s stopped without covering the key space", ErrInvariantViolation, c.target.Schema(), c.target.Table())
	}
	return nil
}

// work is one worker's loop: claim a chunk, copy it in its own transaction,
// land it, and tell the chunker how long it took. It returns nil when the
// chunker has no chunk left and the first error otherwise; a cancelled
// context ends the loop with the context's error, which Run resolves to
// the cancellation's cause.
func (c *Copier) work(ctx context.Context, pool *pgxpool.Pool) error {
	for ctx.Err() == nil {
		chunk, ok, err := c.claim(ctx, pool)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		started := c.opts.Clock.Now()
		inserted, err := c.copyChunk(ctx, pool, chunk)
		if err != nil {
			return errors.Join(err, c.release(chunk))
		}
		if err := c.land(chunk, inserted); err != nil {
			return err
		}
		if err := c.chunker.Feedback(chunk, c.opts.Clock.Now().Sub(started)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// claim cuts the next chunk and registers it in flight before any worker
// reads it, so a Position taken at any later instant shows the chunk as in
// flight until it lands.
func (c *Copier) claim(ctx context.Context, pool *pgxpool.Pool) (Chunk, bool, error) {
	chunk, ok, err := c.chunker.Next(ctx, pool)
	if err != nil || !ok {
		return Chunk{}, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// INV: CO-4
	c.ledger.claim(chunk)
	return chunk, true, nil
}

func (c *Copier) release(chunk Chunk) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// INV: LK-3
	if !c.ledger.release(chunk) {
		return fmt.Errorf("%w (LK-3): released chunk [%d, %d] was not in flight", ErrInvariantViolation, chunk.Lower(), chunk.Upper())
	}
	return nil
}

func (c *Copier) land(chunk Chunk, inserted int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// INV: CO-4
	if !c.ledger.land(chunk, inserted) {
		return fmt.Errorf("%w (LK-3): landed chunk [%d, %d] was not in flight", ErrInvariantViolation, chunk.Lower(), chunk.Upper())
	}
	return nil
}
