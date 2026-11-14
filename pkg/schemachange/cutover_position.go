package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/decode"
)

// readSwapPosition reads the server's WAL insert position inside the swap
// transaction, after the lock and before COMMIT. The ACCESS EXCLUSIVE lock
// has excluded every other session from the source since it was granted,
// so every write to the source committed before the lock and lies below
// this position, and every write to the table's name that commits after
// the swap lies above it. The insert position, not the write or flush
// position, is read because a transaction committed without waiting for
// its flush (synchronous_commit off) may sit past the flushed position.
func readSwapPosition(ctx context.Context, tx pgx.Tx) (decode.LSN, error) {
	var text string
	// INV: CO-9
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_current_wal_insert_lsn()::text`).Scan(&text); err != nil {
		return 0, fmt.Errorf("read the swap's WAL position: %w", err)
	}
	position, err := decode.ParseLSN(text)
	if err != nil {
		return 0, fmt.Errorf("read the swap's WAL position: %w", err)
	}
	return position, nil
}
