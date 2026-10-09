package decode

import (
	"fmt"
	"strconv"
	"strings"
)

// ChangeKind identifies a decoded row operation.
type ChangeKind uint8

const (
	// Insert creates a row.
	Insert ChangeKind = iota + 1
	// Update changes a row.
	Update
	// Delete removes a row.
	Delete
)

// String returns the stable operation name.
func (k ChangeKind) String() string {
	switch k {
	case Insert:
		return "insert"
	case Update:
		return "update"
	case Delete:
		return "delete"
	default:
		return fmt.Sprintf("ChangeKind(%d)", k)
	}
}

// LSN is a PostgreSQL log sequence number.
type LSN uint64

// String renders the PostgreSQL X/Y representation.
func (l LSN) String() string { return fmt.Sprintf("%X/%X", uint64(l)>>32, uint64(l)&0xffffffff) }

// ParseLSN reads the PostgreSQL X/Y representation — the text form of a
// pg_lsn value — back into an LSN. Both halves are hexadecimal and each
// must fit in 32 bits; anything else is an error naming the input.
func ParseLSN(s string) (LSN, error) {
	high, low, ok := strings.Cut(s, "/")
	if !ok {
		return 0, fmt.Errorf("parse LSN %q: want the X/Y form", s)
	}
	hi, err := strconv.ParseUint(high, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("parse LSN %q: high half: %w", s, err)
	}
	lo, err := strconv.ParseUint(low, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("parse LSN %q: low half: %w", s, err)
	}
	return LSN(hi<<32 | lo), nil
}

// Column is one decoded column. Present=false means pgoutput omitted an
// unchanged TOAST value, so an applier must leave the target column untouched.
type Column struct {
	Name    string
	Value   any
	Present bool
}

// ChangeEvent is one decoded row change. OldKey is non-nil only when an update moved the key.
type ChangeEvent struct {
	Kind ChangeKind
	// LSN is the WAL position the change was written at. The server sends
	// a transaction when it commits, so LSN can lie below a position the
	// stream has already delivered or the caller has already confirmed:
	// it orders changes within their transaction, not against the stream.
	LSN LSN
	// Delivered is the stream's delivered position when the change
	// arrived. It lies below the change's commit and never below anything
	// already confirmed, so it is the position a caller may confirm while
	// the change is unapplied: a stream reopened from it replays the
	// change's transaction.
	Delivered LSN
	Key       int64
	OldKey    *int64
	Columns   []Column
}

// PresentColumns returns only values present in the decoded row image.
func (e ChangeEvent) PresentColumns() []Column {
	result := make([]Column, 0, len(e.Columns))
	for _, c := range e.Columns {
		if c.Present {
			result = append(result, c)
		}
	}
	return result
}
