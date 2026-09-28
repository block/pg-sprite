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
// locks or a snapshot for the whole tail.
func (c *Copier) clearAbove(ctx context.Context, pool *pgxpool.Pool, from Watermark) error {
	// INV: CO-4
	lower, done := startAfter(from)
	if done {
		return nil
	}
	batch := c.chunker.Rows()
	for {
		removed, err := c.clearBatch(ctx, pool, lower, batch)
		if err != nil {
			return err
		}
		if removed < batch {
			return nil
		}
	}
}

// clearBatch deletes up to limit shadow rows whose key is at or above lower
// in one guarded transaction and reports how many it removed.
func (c *Copier) clearBatch(ctx context.Context, pool *pgxpool.Pool, lower, limit int64) (int64, error) {
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
	if err := c.guard(ctx, tx); err != nil {
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
