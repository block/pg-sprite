package decode

import "fmt"

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

// Column is one decoded column. Present=false means pgoutput omitted an
// unchanged TOAST value, so an applier must leave the target column untouched.
type Column struct {
	Name    string
	Value   any
	Present bool
}

// ChangeEvent is one decoded row change. OldKey is non-nil only when an update moved the key.
type ChangeEvent struct {
	Kind    ChangeKind
	LSN     LSN
	Key     int64
	OldKey  *int64
	Columns []Column
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
