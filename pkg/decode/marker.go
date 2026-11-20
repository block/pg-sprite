package decode

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MarkerPrefix names the logical messages the engine writes into the WAL
// as position markers; nothing decodes them, and the prefix only tells a
// reader of the WAL whose they are.
const MarkerPrefix = "pgsprite"

// markWALSQL writes the marker and reads the position just past it. It
// runs in a transaction on the caller's pool, whatever its search_path, so
// the cast is spelled through pg_catalog as well as the function: a
// lookalike text type on the path would otherwise reshape the position a
// caller waits for the stream to pass (CO-9).
const markWALSQL = `SELECT pg_catalog.pg_logical_emit_message(false, $1, '')::pg_catalog.text`

// MarkWAL writes a marker into the WAL from inside tx and returns the
// position just past it. Every transaction visible to tx's snapshot
// committed before the marker, so a stream confirmed past the marker's
// position has delivered every change the snapshot saw.
//
// The marker is a non-transactional logical message with an empty payload.
// The server writes it to the WAL at once, outside any transaction, and
// tx's commit — a transaction that wrote WAL but holds no transaction ID —
// asks the WAL writer to flush through it, so the walsender reads past the
// marker within the WAL writer's delay whatever else is open on the
// server. The server's insert position would bound the same commits, but
// it can sit in a WAL tail that nothing flushes while another session
// holds a transaction open after its last write, and a consumer cannot
// confirm what the walsender cannot read. The message names no table and
// the stream does not ask for messages, so the decoder skips it and the
// shadow is not written.
func MarkWAL(ctx context.Context, tx pgx.Tx) (LSN, error) {
	var text string
	if err := tx.QueryRow(ctx, markWALSQL, MarkerPrefix).Scan(&text); err != nil {
		return 0, fmt.Errorf("mark the WAL position: %w", err)
	}
	lsn, err := ParseLSN(text)
	if err != nil {
		return 0, fmt.Errorf("mark the WAL position: %w", err)
	}
	return lsn, nil
}
