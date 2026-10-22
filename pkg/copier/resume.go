package copier

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
)

// clearAbove removes every shadow row whose key lies above the resume
// watermark before the resumed copy cuts its first chunk. The earlier run's
// unlanded chunks may have committed rows there, and the applier treats
// every key above the cut frontier as uncut and discards changes for it, so
// a shadow row left above the watermark would keep a value the source has
// since moved past (CO-4). The clear starts at the first key the resumed
// chunker will cut, so the two agree on what is uncut: after a zero
// watermark that is the whole key space, because a run whose first chunk
// never landed may still have landed chunks anywhere above it, and a first
// run finds the shadow empty and its one batch removes nothing. A watermark
// at the largest key has nothing above it. The clear runs in bounded
// batches, each in its own guarded transaction, so no single statement holds
// locks or a snapshot for the whole tail; the first batch also fences the
// earlier run's straggling chunk transactions, so a row they commit after
// the clear began cannot survive it.
func (c *Copier) clearAbove(ctx context.Context, pool *pgxpool.Pool, from Watermark) error {
	// INV: CO-4
	lower, done := startAfter(from)
	if done {
		return nil
	}
	batch := c.chunker.Rows()
	fence := true
	for {
		removed, err := c.clearBatch(ctx, pool, lower, batch, fence)
		if err != nil {
			return err
		}
		if removed < batch {
			return nil
		}
		fence = false
	}
}

// clearBatch deletes up to limit shadow rows whose key is at or above lower
// in one guarded transaction and reports how many it removed. With fence set
// its guard takes the shadow in the fence's mode, so it waits for the earlier
// run's straggling chunk transactions before the delete reads.
func (c *Copier) clearBatch(ctx context.Context, pool *pgxpool.Pool, lower, limit int64, fence bool) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin clear of %s from %d: %w", c.shadow.ShadowTable(), lower, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := c.guard(ctx, tx, clearShadowLockMode(fence)); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, c.clearSQL, lower, limit)
	if err != nil {
		return 0, fmt.Errorf("clear %s from %d: %w", c.shadow.ShadowTable(), lower, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit clear of %s from %d: %w", c.shadow.ShadowTable(), lower, err)
	}
	return tag.RowsAffected(), nil
}

// clearShadowLockMode is the mode a clear batch's guard takes on the shadow.
// The first batch is the fence: it must wait for every chunk transaction of
// the earlier run that is still writing into the shadow, or still committing
// into it, before its delete reads. Such a transaction holds ROW EXCLUSIVE on
// the shadow from before it confirms the table lock until its commit
// completes — through a wait for a synchronous standby included — and its
// rows are invisible to the clear until then, so a clear that ran ahead of
// it would leave a row above the watermark that the resumed run's own insert
// never overwrites (CO-4). SHARE ROW EXCLUSIVE conflicts with ROW EXCLUSIVE,
// so it is granted only once every straggler has ended and the delete that
// follows, in the same transaction, sees whatever they committed; the wait
// is bounded by the transaction's lock_timeout. It is the guard's one lock
// on the shadow rather than a stronger lock taken after a weaker one, so an
// earlier run stalled inside its own first batch holds the same mode and
// this fence queues behind it instead of deadlocking with it. Only the first
// batch needs the fence: a chunk transaction of the earlier run that
// confirmed the lock before it passed to this one already holds ROW
// EXCLUSIVE, and one that has not yet taken it confirms the lock afterwards
// and is refused. Every later batch holds what its delete does.
func clearShadowLockMode(fence bool) lockMode {
	// INV: CO-4
	if fence {
		return lockShareRowExclusive
	}
	return lockRowExclusive
}

// clearAboveSQL is the one statement every clear batch runs: delete the
// lowest $2 shadow rows whose key is at or above $1. The lower bound is
// inclusive so the smallest key a row can carry is reachable when nothing
// has landed. The subquery orders by the key so each batch removes a
// contiguous stretch off the primary-key index rather than an arbitrary
// sample, and the bounds are declared bigint as the copy statement's are.
func clearAboveSQL(target preflight.CopySwapTarget, shadow Shadow) string {
	key := pgx.Identifier{target.PKColumn()}.Sanitize()
	table := pgx.Identifier{shadow.Schema(), shadow.ShadowTable()}.Sanitize()
	return "DELETE FROM " + table +
		" WHERE " + key + " IN (" +
		"SELECT " + key + " FROM " + table +
		" WHERE " + key + " >= $1::bigint" +
		" ORDER BY " + key +
		" LIMIT $2::bigint)"
}
