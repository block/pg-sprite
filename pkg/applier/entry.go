package applier

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/decode"
)

// EntryKind is what a buffered entry asks the flush to do for its key.
type EntryKind uint8

const (
	// Image writes the row: an upsert of every present column, with any
	// non-present column completed by the flush (CO-8).
	Image EntryKind = iota + 1
	// DeleteMarker removes the row.
	DeleteMarker
)

// String returns the stable kind name.
func (k EntryKind) String() string {
	switch k {
	case Image:
		return "image"
	case DeleteMarker:
		return "delete-marker"
	default:
		return fmt.Sprintf("EntryKind(%d)", k)
	}
}

// Entry is the buffer's latest knowledge of one primary key: at most one per
// key at any flush (CO-5).
type Entry struct {
	Key  int64
	Kind EntryKind
	// OldKey is set on an Image whose row moved since the last flush. It is
	// the key the row started at — the only key whose shadow row can hold
	// the row's pre-buffer version — however many moves the buffer merged,
	// and it equals Key when the row moved away and back. The entry at that
	// key, if still buffered, is flushed in the same batch as this image or
	// deferred with it, never before it, because the flush completes this
	// image's unchanged-TOAST markers before it deletes anything (D13).
	OldKey *int64
	// OldKeyReused is set on an Image with an OldKey when the source has
	// since put another row at that key, so the shadow row the copier reads
	// or has read there is that row's, not this one's pre-buffer version.
	// The flush completes a reused image's markers from the source row under
	// Key, never from the old key's shadow row (D13). Inherited through a
	// chain of moves with OldKey; a row returning to its own origin is not a
	// reuse.
	OldKeyReused bool
	// Columns is the merged row image for an Image, in the decoded column
	// order; a column with Present=false is an unchanged-TOAST marker whose
	// value no buffered event carried. It is nil for a DeleteMarker.
	Columns []decode.Column
	// FirstLSN is the position of the earliest event this entry still holds:
	// a stream confirmed past it would lose the entry on replay.
	FirstLSN decode.LSN
}

// HasMarker reports whether the image still carries a column no buffered
// event supplied, so the flush must complete it (CO-8).
func (e Entry) HasMarker() bool {
	for _, c := range e.Columns {
		if !c.Present {
			return true
		}
	}
	return false
}

// overlay assigns every present column of cols onto the image by name,
// appending a name the image does not hold yet, and leaves the rest alone.
func (e *Entry) overlay(cols []decode.Column) {
	for _, c := range cols {
		if !c.Present {
			continue
		}
		if i := e.index(c.Name); i >= 0 {
			e.Columns[i] = c
			continue
		}
		e.Columns = append(e.Columns, c)
	}
}

func (e Entry) index(name string) int {
	for i, c := range e.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func cloneColumns(cols []decode.Column) []decode.Column {
	if cols == nil {
		return nil
	}
	out := make([]decode.Column, len(cols))
	copy(out, cols)
	return out
}
