package copier

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/internal/chunksql"
	"github.com/block/pg-sprite/pkg/preflight"
)

// copySkipping copies one chunk whose strict insert collided, in its own
// guarded transaction, leaving out every source row whose unique value a
// shadow row still holds under another key. It returns the rows it inserted
// and the ascending keys it held off — the rows the chunk still owes, which
// copyKeys lands once the catch-up has rewritten the rows holding their
// values (CO-4).
func (c *Copier) copySkipping(ctx context.Context, pool *pgxpool.Pool, chunk Chunk) (int64, []int64, error) {
	var inserted int64
	var held []int64
	err := c.chunkTx(ctx, pool, chunk, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, c.skipSQL, chunk.Lower(), chunk.Upper()).Scan(&inserted, &held); err != nil {
			return fmt.Errorf("copy chunk [%d, %d] of %s.%s into %s around the values other shadow rows hold: %w", chunk.Lower(), chunk.Upper(), c.shadow.Schema(), c.shadow.SourceTable(), c.shadow.ShadowTable(), err)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return inserted, held, nil
}

// copyKeys copies the source rows under keys, the keys a chunk's earlier pass
// held off, in its own guarded transaction, again leaving out any row whose
// value a shadow row still holds. It reads only those keys: a source row
// that arrived in the chunk since the pass that held them off is the
// catch-up's to write, as the rest of the chunk has landed. It returns the
// rows it inserted and the keys still held off, a subset of keys.
func (c *Copier) copyKeys(ctx context.Context, pool *pgxpool.Pool, chunk Chunk, keys []int64) (int64, []int64, error) {
	var inserted int64
	var held []int64
	err := c.chunkTx(ctx, pool, chunk, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, c.keysSQL, keys).Scan(&inserted, &held); err != nil {
			return fmt.Errorf("copy the %d held-off rows of chunk [%d, %d] of %s.%s into %s: %w", len(keys), chunk.Lower(), chunk.Upper(), c.shadow.Schema(), c.shadow.SourceTable(), c.shadow.ShadowTable(), err)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return inserted, held, nil
}

// copySkipSQL is the statement a collided chunk runs over its whole range,
// built for this target and shadow.
func copySkipSQL(target preflight.CopySwapTarget, shadow Shadow) string {
	return chunksql.InsertSkipping(shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), target.PKColumn(), shadow.CopyColumns(), shadow.Conversions())
}

// copyKeysSQL is the statement a collided chunk runs over the keys it still
// owes, built for this target and shadow.
func copyKeysSQL(target preflight.CopySwapTarget, shadow Shadow) string {
	return chunksql.InsertKeys(shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), target.PKColumn(), shadow.CopyColumns(), shadow.Conversions())
}
