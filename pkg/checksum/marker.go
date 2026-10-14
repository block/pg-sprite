package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/decode"
)

// markerPrefix names the logical messages a live pass writes; nothing
// decodes them, and the prefix only tells a reader of the WAL whose they
// are.
const markerPrefix = "pgsprite"

// markPosition writes a marker into the WAL from inside tx and returns the
// position just past it. Every transaction visible to tx's snapshot
// committed before the marker, so once the catch-up has confirmed past the
// marker's position, every change the snapshot saw is in the shadow.
//
// The marker is a non-transactional logical message with an empty payload.
// The server writes it to the WAL at once, outside any transaction, and
// tx's commit — a transaction that wrote WAL but holds no transaction ID —
// asks the WAL writer to flush through it, so the walsender reads past the
// marker within the WAL writer's delay whatever else is open on the
// server. The server's insert position would bound the same commits, but
// it can sit in a WAL tail that nothing flushes while another session
// holds a transaction open after its last write, and a catch-up cannot
// confirm what the walsender cannot read. The message names no table and
// the stream does not ask for messages, so the decoder skips it and the
// shadow is not written.
func (v *Verifier) markPosition(ctx context.Context, tx pgx.Tx) (decode.LSN, error) {
	var text string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_logical_emit_message(false, $1, '')::text`, markerPrefix).Scan(&text); err != nil {
		return 0, fmt.Errorf("mark the WAL position of a read of %s.%s: %w", v.target.Schema(), v.target.Table(), err)
	}
	lsn, err := decode.ParseLSN(text)
	if err != nil {
		return 0, fmt.Errorf("mark the WAL position of a read of %s.%s: %w", v.target.Schema(), v.target.Table(), err)
	}
	return lsn, nil
}
