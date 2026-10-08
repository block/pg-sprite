package decode

import (
	"context"
	"fmt"

	"github.com/jackc/pglogrepl"
)

// Confirmed is the position last reported to the server as applied; zero
// before the first Confirm.
func (s *Stream) Confirmed() LSN { return s.confirmed }

// Confirm reports to the server that every transaction that committed at or
// below lsn has been applied durably, which lets the slot release the WAL
// below it and makes lsn the point a later stream resumes from. The position
// must not fall below an earlier confirmation and must not exceed what the
// stream has delivered: confirming further would let the server discard a
// transaction the caller never saw. A change whose own LSN lies below lsn
// is not thereby covered — only its commit is — so the position a caller
// confirms while changes are unapplied is the Delivered the earliest of them
// arrived with. The server itself never moves the slot's position
// backwards, so confirming a position below the slot's current one — which
// a resumed stream can produce from a replayed transaction's early changes
// — is accepted and has no effect on the server. A position the server
// could not be told ends the stream, since the connection can no longer
// carry a report; the record of what was confirmed is left where it was.
func (s *Stream) Confirm(ctx context.Context, lsn LSN) error {
	if s.failed != nil {
		return s.failed
	}
	// INV: ST-4 — the slot's confirmed position moves only on the caller's
	// word and never past what the caller could have applied.
	if lsn < s.confirmed {
		return fmt.Errorf("%w: ST-4: confirm %s below the %s already confirmed on slot %s",
			ErrInvariantViolation, lsn, s.confirmed, s.slotName)
	}
	if lsn > s.delivered {
		return fmt.Errorf("%w: ST-4: confirm %s beyond the %s delivered from slot %s",
			ErrInvariantViolation, lsn, s.delivered, s.slotName)
	}
	if err := s.sendStatus(ctx, lsn); err != nil {
		return s.fail(err)
	}
	s.confirmed = lsn
	return nil
}

// sendStatus reports a confirmed position to the server as the write, flush,
// and apply positions alike, so the report carries exactly that position
// whatever a library default would fill in for one left zero. Before the
// first Confirm every position is zero, which the server reads as no
// position: the message still counts as the reply a keepalive asked for.
func (s *Stream) sendStatus(ctx context.Context, confirmed LSN) error {
	err := pglogrepl.SendStandbyStatusUpdate(ctx, s.conn, pglogrepl.StandbyStatusUpdate{
		WALWritePosition: pglogrepl.LSN(confirmed),
		WALFlushPosition: pglogrepl.LSN(confirmed),
		WALApplyPosition: pglogrepl.LSN(confirmed),
	})
	if err != nil {
		return fmt.Errorf("confirm %s on slot %s: %w", confirmed, s.slotName, err)
	}
	return nil
}
