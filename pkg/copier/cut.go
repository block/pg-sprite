package copier

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// cut asks the chunker for the next chunk inside a bounded read-only
// transaction of its own. The boundary query takes ACCESS SHARE on the
// source, and the connection is the caller's, so the copier does not know
// what session defaults the pool carries: the transaction bounds itself
// before it reads (LK-2), and a cut queued behind a strong lock on the
// source ends at the copier's lock timeout, not at the lock holder's
// patience. The chunker moves its cursor as soon as the boundary is read,
// before the transaction commits; a commit that then fails ends the run, and
// a resumed run rebuilds the chunker from the landed watermark, so a cursor
// moved past an uncommitted cut is never copied from.
func (c *Copier) cut(ctx context.Context, pool *pgxpool.Pool) (chunk Chunk, ok bool, err error) {
	err = c.readBounded(ctx, pool, func(tx pgx.Tx) error {
		chunk, ok, err = c.chunker.Next(ctx, tx)
		return err
	})
	if err != nil {
		return Chunk{}, false, err
	}
	return chunk, ok, nil
}

// readBounded runs read inside a read-only transaction under the chunk
// budgets with the catalog alone on its search_path, and commits it. The
// cut's key comparison resolves its operator through the search_path, so
// the pin keeps a schema ahead of pg_catalog on the caller's pool from
// answering it (CO-9); the measurements name every catalog object with its
// schema and take the same pin for the same session shape. Read-only
// because nothing here writes, and the server holds it to that.
func (c *Copier) readBounded(ctx context.Context, pool *pgxpool.Pool, read func(tx pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin bounded read: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBudgets(ctx, tx, c.opts); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return fmt.Errorf("set bounded read search_path: %w", err)
	}
	if err := read(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
