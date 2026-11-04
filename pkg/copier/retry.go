package copier

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQLSTATE codes the server raises when a chunk's insert lands a row whose
// value another shadow row already holds under a unique index or an
// exclusion constraint.
const (
	sqlstateUniqueViolation    = "23505"
	sqlstateExclusionViolation = "23P01"
)

// sqlstateDeadlockDetected is the SQLSTATE the server raises when it breaks
// a cycle of transactions waiting on each other by rolling this one back.
const sqlstateDeadlockDetected = "40P01"

// sqlstateObjectNotInPrerequisiteState is the SQLSTATE the server raises,
// among other cases, when an INSERT ... ON CONFLICT with no conflict target
// meets a table with a DEFERRABLE unique or exclusion constraint, which
// cannot serve as an arbiter.
const sqlstateObjectNotInPrerequisiteState = "55000"

const (
	// DefaultCollisionAttempts is the number of passes a chunk whose insert
	// collided is given before the copy stops, when Options leaves
	// CollisionAttempts zero.
	DefaultCollisionAttempts = 10
	// DefaultCollisionBackoff is the wait before a collided chunk's first
	// pass over the rows it held off when Options leaves CollisionBackoff
	// zero. Each later wait is one backoff longer, so the default passes
	// together outlast many catch-up cycles, and a cycle's flush is what
	// moves the colliding value out of the held-off rows' way.
	DefaultCollisionBackoff = 250 * time.Millisecond
)

// collided reports whether err is the server refusing a chunk's insert
// because a shadow row already holds one of its values under a unique index
// or an exclusion constraint. Only the strict insert raises it, and the
// primary key cannot be the index: that insert's ON CONFLICT (pk) DO NOTHING
// skips a row already there (CO-4). The passes that copy around held-off
// rows use ON CONFLICT DO NOTHING with no target, which skips a conflict on
// any unique index or exclusion constraint instead of raising it.
func collided(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlstateUniqueViolation || pgErr.Code == sqlstateExclusionViolation
}

// deadlocked reports whether err is the server rolling a chunk's pass back
// to break a deadlock. The shadow's writers — the copier's workers and the
// applier's flush — insert values under the same unique indexes, so while
// source changes move values between keys two of them can each wait on a
// value the other's open transaction holds: the cyclic form of a
// collision. The rolled-back pass committed nothing, so the chunk is given
// another pass on the same terms as a collision. The match is by SQLSTATE
// alone, so a deadlock on any statement of the pass — the guard's table
// locks included — earns the pass too; that is as safe, since the pass
// still committed nothing. Only this side of the cycle is retried: a flush
// the server picks as the victim fails the catch-up.
func deadlocked(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlstateDeadlockDetected
}

// earnsAnotherPass reports whether a chunk's strict insert ended in a way
// that gives the chunk its collision passes: a collision, or a deadlock
// with another shadow writer.
func earnsAnotherPass(err error) bool {
	if collided(err) {
		return true
	}
	return deadlocked(err)
}

// refusesSkipping reports whether err is the server refusing the pass that
// copies around held-off rows because the shadow has a DEFERRABLE unique or
// exclusion constraint, which ON CONFLICT with no target cannot use as an
// arbiter.
func refusesSkipping(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlstateObjectNotInPrerequisiteState
}

// copyUntilClear copies chunk until every row of it has committed, returning
// the rows the passes inserted in all and how long the pass that committed
// the range took, or until the copy must stop. A collision is a row the
// chunk read from the source whose unique value a shadow row still holds
// under another key: a source change moved the value between the keys while
// this chunk was in flight, and the applier's flush has yet to rewrite the
// landed row the value came from, or the chunk read both rows before the
// move and the flush has since given a landed row the value the chunk
// carries (CO-4). The first pass is the strict insert, which collides as a
// whole; it serves the chunk that collides on nothing, the common case, at
// one statement. A pass the server rolls back to break a deadlock is
// treated as a collision (deadlocked). Nothing else is retried here: a row
// the shadow rejects on its own terms, a lock wait that timed out, a lost
// connection is the first pass's error, returned as such.
func (c *Copier) copyUntilClear(ctx context.Context, pool *pgxpool.Pool, chunk Chunk) (int64, time.Duration, error) {
	started := c.opts.Clock.Now()
	inserted, err := c.copyChunk(ctx, pool, chunk)
	if err == nil {
		return inserted, c.opts.Clock.Now().Sub(started), nil
	}
	if !earnsAnotherPass(err) {
		return 0, 0, err
	}
	c.collisions.Add(1)
	return c.copyAroundCollisions(ctx, pool, chunk, err)
}

// copyAroundCollisions finishes a chunk whose strict insert collided, in
// passes numbered after it up to Options.CollisionAttempts. The first pass
// commits every row of the chunk but those whose values other shadow rows
// hold and settles the chunk with those keys pending, so the applier writes
// the rest of the chunk from then on; each later pass, after a wait one
// backoff longer than the one before, copies only the rows still pending.
// Each pass is its own transaction with its own snapshot, so once the flush
// has rewritten the row holding a pending row's value the next pass lands
// it, and a pending row the source has since deleted is simply no longer
// read; the pending keys only ever shrink, and the chunk lands when they
// are gone. A shadow with a DEFERRABLE unique or exclusion constraint
// cannot be copied around, since the server refuses the pass's target-less
// ON CONFLICT on it, so its chunk reruns the strict insert over the whole
// range instead (rerunWhole). A chunk no pass lands ends the copy with the
// latest pass's error from the server — a collision or a deadlock —
// wrapped with the chunk's bounds, the passes made, and the keys still
// pending; the chunk stays in flight with those keys pending.
func (c *Copier) copyAroundCollisions(ctx context.Context, pool *pgxpool.Pool, chunk Chunk, collision error) (int64, time.Duration, error) {
	var total int64
	var took time.Duration
	// pending is nil until the pass over the whole range commits.
	var pending []int64
	for pass := 2; ; pass++ {
		if pass == c.opts.CollisionAttempts+1 {
			return 0, 0, c.pendingOutlasted(chunk, pending, collision)
		}
		if err := c.waitBeforePass(ctx, pass); err != nil {
			return 0, 0, err
		}
		started := c.opts.Clock.Now()
		inserted, held, err := c.copyPending(ctx, pool, chunk, pending)
		if deadlocked(err) {
			// The pass committed nothing, so pending is unchanged; the
			// deadlock is now the error an exhausted chunk reports.
			collision = err
			c.collisions.Add(1)
			continue
		}
		if err != nil {
			if pending == nil && refusesSkipping(err) {
				return c.rerunWhole(ctx, pool, chunk, pass+1, collision)
			}
			return 0, 0, err
		}
		if pending == nil {
			took = c.opts.Clock.Now().Sub(started)
		}
		total += inserted
		if len(held) == 0 {
			return total, took, nil
		}
		pending = held
		if err := c.settle(chunk, total, pending); err != nil {
			return 0, 0, err
		}
		c.collisions.Add(1)
	}
}

// rerunWhole finishes a collided chunk whose shadow cannot be copied around,
// in passes numbered from pass up to Options.CollisionAttempts: each waits
// as waitBeforePass says and then runs the strict insert over the whole
// range, which lands every row of the chunk once the flush has moved the
// colliding value out of its way and otherwise collides again. Nothing is
// settled, so the chunk's keys stay in flight until it lands. A pass the
// server rolls back to break a deadlock takes the next pass, as a
// collision does. A chunk no pass lands ends the copy with the latest
// pass's error, wrapped with the chunk's bounds and the passes made.
func (c *Copier) rerunWhole(ctx context.Context, pool *pgxpool.Pool, chunk Chunk, pass int, collision error) (int64, time.Duration, error) {
	for ; pass <= c.opts.CollisionAttempts; pass++ {
		if err := c.waitBeforePass(ctx, pass); err != nil {
			return 0, 0, err
		}
		started := c.opts.Clock.Now()
		inserted, err := c.copyChunk(ctx, pool, chunk)
		if err == nil {
			return inserted, c.opts.Clock.Now().Sub(started), nil
		}
		if !earnsAnotherPass(err) {
			return 0, 0, err
		}
		collision = err
		c.collisions.Add(1)
	}
	return 0, 0, c.pendingOutlasted(chunk, nil, collision)
}

// pendingOutlasted is the error that ends the copy when a chunk has used
// its passes: it names how many keys are still pending and the lowest of
// them, which Position().Pending lists in full, and wraps the latest pass's
// error, a collision or a deadlock, so errors.As finds the SQLSTATE that
// ended the run.
func (c *Copier) pendingOutlasted(chunk Chunk, pending []int64, last error) error {
	if len(pending) == 0 {
		return fmt.Errorf("chunk [%d, %d] did not land in %d attempts: %w", chunk.Lower(), chunk.Upper(), c.opts.CollisionAttempts, last)
	}
	return fmt.Errorf("chunk [%d, %d] still has %d pending keys after %d attempts, the lowest %d: %w", chunk.Lower(), chunk.Upper(), len(pending), c.opts.CollisionAttempts, pending[0], last)
}

// copyPending runs one pass of a collided chunk: over the whole range while
// no pass has committed around the held-off rows yet (pending nil), and
// over the pending keys alone after that.
func (c *Copier) copyPending(ctx context.Context, pool *pgxpool.Pool, chunk Chunk, pending []int64) (int64, []int64, error) {
	if pending == nil {
		return c.copySkipping(ctx, pool, chunk)
	}
	return c.copyKeys(ctx, pool, chunk, pending)
}

// waitBeforePass waits before a collided chunk's pass: nothing before the
// pass that commits around the held-off rows, since it needs no change in
// the shadow to make progress, and one backoff more before each pass after
// it, since those wait on the catch-up's flush.
func (c *Copier) waitBeforePass(ctx context.Context, pass int) error {
	if pass == 2 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.opts.CollisionBackoff * time.Duration(pass-2)):
		return nil
	}
}

// Collisions is the number of chunk passes this copier's run has had to
// repeat: passes the server refused on a unique index or an exclusion
// constraint, passes it rolled back to break a deadlock, and passes that
// left rows held off, each followed by another pass or by the copy
// stopping. It mixes both causes in one count; the error a stopped copy
// returns names the one that ended it. It counts per pass, so one chunk
// waiting on a single held-off key adds one for each pass that still finds
// the key's value taken. It is safe to read from any goroutine while Run
// is in progress; a copy whose source saw no value move during it reads
// zero.
func (c *Copier) Collisions() int64 {
	return c.collisions.Load()
}
