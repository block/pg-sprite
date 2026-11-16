package change

import (
	"context"
	"fmt"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// formatPosition renders an LSN as a position; the zero LSN is "", the
// position of a source that has observed nothing.
func formatPosition(lsn decode.LSN) string {
	if lsn == 0 {
		return ""
	}
	return lsn.String()
}

// parsePosition reads a position back into an LSN.
func parsePosition(pos string) (decode.LSN, error) {
	if pos == "" {
		return 0, fmt.Errorf("%w: an empty position names nothing to resume from; use Start", ErrPositionNotFound)
	}
	lsn, err := decode.ParseLSN(pos)
	if err != nil {
		return 0, fmt.Errorf("position %q: %w", pos, err)
	}
	return lsn, nil
}

// requireResumable checks that the slot a resume reads from still exists and
// still holds its WAL. A slot that has confirmed past the position is
// resumable: only flushed positions are confirmed, so what the server skips
// is already on the target.
func requireResumable(ctx context.Context, db dbconn.RowQuerier, slot string, from decode.LSN) error {
	status, found, err := decode.InspectSlot(ctx, db, slot)
	if err != nil {
		return fmt.Errorf("inspect slot %s: %w", slot, err)
	}
	if !found {
		return fmt.Errorf("%w: slot %s does not exist", ErrPositionNotFound, slot)
	}
	if !status.Own {
		return fmt.Errorf("%w: slot %s belongs to database %q", decode.ErrForeignDecodingState, slot, status.Database)
	}
	if status.Lost() {
		return fmt.Errorf("%w: slot %s at %s: %w", ErrPositionNotFound, slot, from, status.LostState())
	}
	return nil
}
