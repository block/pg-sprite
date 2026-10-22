package applier

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/decode"
)

// shadowKeysSQL reads which of the bigint keys $1 have a row in the shadow
// relation, and last the server's WAL insert position as text. The position
// is taken in the same statement as the read, as rowSQL takes it, so the
// buffer can tell when the stream has delivered everything the read knew
// about.
func shadowKeysSQL(relation, pk string) string {
	return "SELECT ARRAY(SELECT " + pk + "::bigint FROM " + relation + " WHERE " + pk + " = ANY($1::bigint[])), " +
		"pg_catalog.pg_current_wal_insert_lsn()::text"
}

// checksShadowRow reports whether the flush must find the shadow row under the
// image's own key before it writes: a marker-bearing image of a row that did
// not move, whose marker stands for a value in that row, and any image that
// already waited out one absence (AbsenceAwaited), even if later UPDATEs have
// since filled in every marker, since the stream then still holds the row live
// under a key the shadow has lost. A moved image's value lives under another
// key and is completed from there.
func (f *Flusher) checksShadowRow(e *Entry) bool {
	if e.Kind != Image || e.OldKey != nil {
		return false
	}
	if e.AbsenceAwaited {
		return true
	}
	_, markers := f.split(e)
	return len(markers) > 0
}

// holdAbsent takes out of the entries every marker-bearing image of a row
// that did not move whose shadow row is absent, and holds it in the result
// without a completion (D13); an image already flagged AbsenceAwaited is
// checked whether or not it still carries a marker. The row is absent
// legitimately when the source deleted it before the chunk covering its key
// was read: the chunk never
// copied it and the DELETE has not reached the stream yet. Nothing is written
// for the image; the buffer lets it back in once the stream has passed the
// position this check read, by which time that DELETE, or an INSERT of the
// key, has replaced the image. An image that comes back with AbsenceAwaited
// set and the row still absent is one the stream holds live under a key the
// shadow lost, and the flush refuses it. The batch's own entries are left as
// they are, so Requeue puts the image back as the stream delivered it.
func (f *Flusher) holdAbsent(ctx context.Context, tx pgx.Tx, entries []Entry, result *Result) ([]Entry, error) {
	var keys []int64
	for i := range entries {
		if f.checksShadowRow(&entries[i]) {
			keys = append(keys, entries[i].Key)
		}
	}
	if len(keys) == 0 {
		return entries, nil
	}
	present, readLSN, err := f.shadowKeys(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	kept := make([]Entry, 0, len(entries))
	for i := range entries {
		e := entries[i]
		if !f.checksShadowRow(&e) {
			kept = append(kept, e)
			continue
		}
		if _, found := present[e.Key]; found {
			kept = append(kept, e)
			continue
		}
		if e.AbsenceAwaited {
			// INV: CO-8
			return nil, fmt.Errorf("%w (CO-8): flush key %d: the shadow holds no row for an image the stream still holds live, though the stream has passed the check that found it absent", ErrInvariantViolation, e.Key)
		}
		e.Columns = cloneColumns(e.Columns)
		e.AbsenceAwaited = true
		result.Held = append(result.Held, HeldImage{Entry: e, ReadLSN: readLSN})
	}
	return kept, nil
}

// shadowKeys reads which of keys have a shadow row, with the WAL position of
// the read.
func (f *Flusher) shadowKeys(ctx context.Context, tx pgx.Tx, keys []int64) (map[int64]struct{}, decode.LSN, error) {
	var found []int64
	var readAt string
	if err := tx.QueryRow(ctx, shadowKeysSQL(f.shadowRelation(), f.pk()), keys).Scan(&found, &readAt); err != nil {
		return nil, 0, fmt.Errorf("read which of %d keys the shadow %s holds: %w", len(keys), f.shadowRelation(), err)
	}
	readLSN, err := decode.ParseLSN(readAt)
	if err != nil {
		return nil, 0, fmt.Errorf("read which of %d keys the shadow %s holds: read position: %w", len(keys), f.shadowRelation(), err)
	}
	present := make(map[int64]struct{}, len(found))
	for _, key := range found {
		present[key] = struct{}{}
	}
	return present, readLSN, nil
}
