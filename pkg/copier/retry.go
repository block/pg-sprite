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
	// DefaultCollisionAttempts is the number of times a chunk whose insert
	// collided is run before the copy stops, when Options leaves
	// CollisionAttempts zero.
	DefaultCollisionAttempts = 10
	// DefaultCollisionBackoff is the wait before a collided chunk's second
	// attempt when Options leaves CollisionBackoff zero. Each later wait is
	// one backoff longer, so the default attempts together outlast many
	// catch-up cycles, and a cycle's flush is what moves the colliding
	// value out of the chunk's way.
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

// copyUntilClear runs chunk's copy until it commits, returning the rows the
// committed attempt inserted and how long that attempt took, or until the
// copy must stop. A collision is a row the chunk read from the source whose
// unique value a shadow row still holds under another key: a source change
// moved the value between the keys while this chunk was in flight, and the
// applier's flush has yet to rewrite the landed row the value came from, or
// the chunk read both rows before the move and the flush has since given a
// landed row the value the chunk carries (CO-4). Each attempt is its own
// transaction with its own snapshot, so once the flush has committed a later
// attempt reads the moved rows as the source now has them and lands. A
// collision no attempt outlasts ends the copy with the
// server's error, wrapped with the chunk's bounds and the attempts made;
// the chunk stays in flight. Nothing else is retried here: a row the shadow
// rejects on its own terms, a lock wait that timed out, a lost connection
// is the first attempt's error, returned as such.
func (c *Copier) copyUntilClear(ctx context.Context, pool *pgxpool.Pool, chunk Chunk) (int64, time.Duration, error) {
	for attempt := 1; ; attempt++ {
		started := c.opts.Clock.Now()
		inserted, err := c.copyChunk(ctx, pool, chunk)
		if err == nil {
			return inserted, c.opts.Clock.Now().Sub(started), nil
		}
		if !collided(err) {
			return 0, 0, err
		}
		c.collisions.Add(1)
		if attempt == c.opts.CollisionAttempts {
			return 0, 0, fmt.Errorf("chunk [%d, %d] collided with a shadow row on each of %d attempts; the row holding its value was not removed between them: %w", chunk.Lower(), chunk.Upper(), attempt, err)
		}
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-time.After(c.opts.CollisionBackoff * time.Duration(attempt)):
		}
	}
}

// Collisions is the number of chunk attempts this copier's run has had the
// server refuse on a unique index or an exclusion constraint, each followed
// by a wait and another attempt or by the copy stopping. It is safe to read
// from any goroutine while Run is in progress; a copy whose source saw no
// value move during it reads zero.
func (c *Copier) Collisions() int64 {
	return c.collisions.Load()
}
