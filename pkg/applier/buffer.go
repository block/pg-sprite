package applier

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// ErrInvariantViolation aliases dbconn's fail-closed error class so one
// errors.Is check covers a breach raised here or in the connection layer.
var ErrInvariantViolation = dbconn.ErrInvariantViolation

// Buffer holds the latest knowledge of every primary key captured since the
// last flush: one Entry per key, merged rather than replaced, so a flush
// applies each key once with its newest image or deletion (CO-5). It is not
// safe for concurrent use; the stream owner adds and drains it from one
// goroutine.
type Buffer struct {
	entries map[int64]*Entry
}

// NewBuffer returns an empty buffer.
func NewBuffer() *Buffer { return &Buffer{entries: make(map[int64]*Entry)} }

// Len reports how many keys are buffered, including entries a Drain deferred.
func (b *Buffer) Len() int { return len(b.entries) }

// OldestPending returns the earliest FirstLSN among buffered entries. A
// stream confirmed at or past it could not replay those entries after a
// restart, so the confirmed position must stay below it. The second result
// is false when the buffer is empty.
func (b *Buffer) OldestPending() (decode.LSN, bool) {
	var oldest decode.LSN
	found := false
	for _, e := range b.entries {
		if !found || e.FirstLSN < oldest {
			oldest = e.FirstLSN
			found = true
		}
	}
	return oldest, found
}

// Add merges one decoded change into the buffer in stream order. An event the
// source could not have produced given what is already buffered — an UPDATE
// or INSERT over a deleted row, an INSERT over a live one, an INSERT missing a
// value, a move onto a live key or from a deleted one — returns
// ErrInvariantViolation, and the buffer is left as it was.
func (b *Buffer) Add(ev decode.ChangeEvent) error {
	// INV: CO-5, CO-8
	switch ev.Kind {
	case decode.Insert:
		return b.addInsert(ev)
	case decode.Update:
		if ev.OldKey != nil && *ev.OldKey != ev.Key {
			return b.addKeyMove(ev)
		}
		return b.addUpdate(ev)
	case decode.Delete:
		b.addDelete(ev)
		return nil
	default:
		return fmt.Errorf("%w (CO-5): buffer key %d: unknown change kind %s", ErrInvariantViolation, ev.Key, ev.Kind)
	}
}

// addInsert creates a complete image; an INSERT carries every column's value
// (CO-8) and can only follow a deletion or nothing at all.
func (b *Buffer) addInsert(ev decode.ChangeEvent) error {
	cur := b.entries[ev.Key]
	if cur != nil && cur.Kind == Image {
		return fmt.Errorf("%w (CO-5): buffer key %d: insert over a buffered image", ErrInvariantViolation, ev.Key)
	}
	for _, c := range ev.Columns {
		if !c.Present {
			return fmt.Errorf("%w (CO-8): buffer key %d: insert omits column %s", ErrInvariantViolation, ev.Key, c.Name)
		}
	}
	b.entries[ev.Key] = &Entry{Key: ev.Key, Kind: Image, Columns: cloneColumns(ev.Columns), FirstLSN: firstLSN(ev.LSN, cur)}
	return nil
}

// addUpdate overlays the event's present columns onto the buffered image for
// the key, so a column the event omitted keeps the value an earlier event
// supplied — or stays a marker when none did (CO-8). An UPDATE of a key the
// buffer holds deleted cannot have happened on the source.
func (b *Buffer) addUpdate(ev decode.ChangeEvent) error {
	cur, ok := b.entries[ev.Key]
	if !ok {
		b.entries[ev.Key] = &Entry{Key: ev.Key, Kind: Image, Columns: cloneColumns(ev.Columns), FirstLSN: ev.LSN}
		return nil
	}
	if cur.Kind == DeleteMarker {
		return fmt.Errorf("%w (CO-5): buffer key %d: update over a buffered delete", ErrInvariantViolation, ev.Key)
	}
	cur.overlay(ev.Columns)
	return nil
}

// addKeyMove enters an UPDATE that moved the primary key as two entries: a
// delete marker at the old key and an image at the new key that remembers the
// old key (CO-5). The new image starts from the old key's buffered image when
// there is one, so a column the move's event omitted keeps a value an earlier
// event at the old key supplied, and overlays the move's present columns. The
// source cannot move a row from a key it has deleted or onto a key that is
// still live.
func (b *Buffer) addKeyMove(ev decode.ChangeEvent) error {
	oldKey := *ev.OldKey
	cur := b.entries[ev.Key]
	if cur != nil && cur.Kind == Image {
		return fmt.Errorf("%w (CO-5): buffer key %d: key move from %d onto a buffered image", ErrInvariantViolation, ev.Key, oldKey)
	}
	from := b.entries[oldKey]
	if from != nil && from.Kind == DeleteMarker {
		return fmt.Errorf("%w (CO-5): buffer key %d: key move from %d, which is buffered deleted", ErrInvariantViolation, ev.Key, oldKey)
	}
	moved := &Entry{Key: ev.Key, Kind: Image, OldKey: &oldKey, FirstLSN: firstLSN(ev.LSN, cur, from)}
	if from != nil {
		moved.Columns = cloneColumns(from.Columns)
		moved.overlay(ev.Columns)
	} else {
		moved.Columns = cloneColumns(ev.Columns)
	}
	b.entries[ev.Key] = moved
	b.entries[oldKey] = &Entry{Key: oldKey, Kind: DeleteMarker, FirstLSN: firstLSN(ev.LSN, from)}
	return nil
}

// addDelete replaces whatever the key held with a delete marker: the newest
// fact about the key is that its row is gone (CO-5).
func (b *Buffer) addDelete(ev decode.ChangeEvent) {
	b.entries[ev.Key] = &Entry{Key: ev.Key, Kind: DeleteMarker, FirstLSN: firstLSN(ev.LSN, b.entries[ev.Key])}
}

// firstLSN is the earliest position a new entry still owes the stream: this
// event's, or an earlier one carried by any entry it replaces or continues.
func firstLSN(lsn decode.LSN, replaced ...*Entry) decode.LSN {
	for _, e := range replaced {
		if e != nil && e.FirstLSN < lsn {
			lsn = e.FirstLSN
		}
	}
	return lsn
}
