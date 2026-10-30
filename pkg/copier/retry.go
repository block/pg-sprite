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
// or an exclusion constraint. The primary key cannot be the index: the
// insert's ON CONFLICT (pk) DO NOTHING skips a row already there (CO-4).
func collided(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlstateUniqueViolation || pgErr.Code == sqlstateExclusionViolation
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
// one statement. Nothing else is retried here: a row the shadow rejects on
// its own terms, a lock wait that timed out, a lost connection is the first
// pass's error, returned as such.
func (c *Copier) copyUntilClear(ctx context.Context, pool *pgxpool.Pool, chunk Chunk) (int64, time.Duration, error) {
	started := c.opts.Clock.Now()
	inserted, err := c.copyChunk(ctx, pool, chunk)
	if err == nil {
		return inserted, c.opts.Clock.Now().Sub(started), nil
	}
	if !collided(err) {
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
// are gone. A pass the server refuses as a whole, as a deferred unique
// constraint refuses at commit, is run again as it was. A collision no pass
// outlasts ends the copy with the server's error, wrapped with the chunk's
// bounds and the passes made; the chunk stays in flight with its keys
// pending.
func (c *Copier) copyAroundCollisions(ctx context.Context, pool *pgxpool.Pool, chunk Chunk, collision error) (int64, time.Duration, error) {
	var total int64
	var took time.Duration
	// pending is nil until the pass over the whole range commits.
	var pending []int64
	for pass := 2; ; pass++ {
		if pass == c.opts.CollisionAttempts+1 {
			return 0, 0, fmt.Errorf("chunk [%d, %d] collided with a shadow row on each of %d attempts and still has rows pending whose values other shadow rows hold: %w", chunk.Lower(), chunk.Upper(), c.opts.CollisionAttempts, collision)
		}
		if err := c.waitBeforePass(ctx, pass); err != nil {
			return 0, 0, err
		}
		started := c.opts.Clock.Now()
		inserted, held, err := c.copyPending(ctx, pool, chunk, pending)
		if err != nil {
			if !collided(err) {
				return 0, 0, err
			}
			collision = err
			c.collisions.Add(1)
			continue
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

// Collisions is the number of chunk passes this copier's run has had the
// server refuse on a unique index or an exclusion constraint, or has had to
// leave rows held off from, each followed by another pass or by the copy
// stopping. It is safe to read from any goroutine while Run is in progress;
// a copy whose source saw no value move during it reads zero.
func (c *Copier) Collisions() int64 {
	return c.collisions.Load()
}
