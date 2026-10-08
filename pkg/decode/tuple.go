package decode

import (
	"fmt"
	"strconv"

	"github.com/jackc/pglogrepl"
)

// decodeColumns turns a pgoutput tuple into the event's columns, one per
// relation column in relation order. A text value is carried as a string
// and NULL as nil, both present; the unchanged-TOAST marker is the one
// column that is not present, so an applier leaves the stored value alone
// (CO-8). The stream asks for text, so a binary value is state pgoutput
// cannot have sent it.
func decodeColumns(rel *relation, tuple *pglogrepl.TupleData) ([]Column, error) {
	if tuple == nil {
		return nil, fmt.Errorf("%w: CO-8: change carries no tuple", ErrInvariantViolation)
	}
	if len(tuple.Columns) != len(rel.columns) {
		return nil, fmt.Errorf("%w: CO-8: tuple has %d columns, the relation has %d",
			ErrInvariantViolation, len(tuple.Columns), len(rel.columns))
	}
	columns := make([]Column, len(rel.columns))
	for i, c := range tuple.Columns {
		col := Column{Name: rel.columns[i].Name}
		switch c.DataType {
		case pglogrepl.TupleDataTypeText:
			col.Value, col.Present = string(c.Data), true
		case pglogrepl.TupleDataTypeNull:
			col.Present = true
		case pglogrepl.TupleDataTypeToast:
			// INV: CO-8 — the marker is carried as absence, never as a value.
		default:
			return nil, fmt.Errorf("%w: CO-8: column %s arrived as tuple data type %q, the stream asked for text",
				ErrInvariantViolation, col.Name, c.DataType)
		}
		columns[i] = col
	}
	return columns, nil
}

// decodeKey reads the primary key out of a tuple. A key-only old tuple sends
// every other column as NULL, so only the key column is read; it is always
// a text value because a key cannot be NULL and a key column is never
// stored out of line.
func decodeKey(rel *relation, tuple *pglogrepl.TupleData) (int64, error) {
	if tuple == nil {
		return 0, fmt.Errorf("%w: CO-5: change carries no tuple to read the key from", ErrInvariantViolation)
	}
	if len(tuple.Columns) != len(rel.columns) {
		return 0, fmt.Errorf("%w: CO-5: tuple has %d columns, the relation has %d",
			ErrInvariantViolation, len(tuple.Columns), len(rel.columns))
	}
	c := tuple.Columns[rel.keyIndex]
	if c.DataType != pglogrepl.TupleDataTypeText {
		return 0, fmt.Errorf("%w: CO-5: key column %s arrived as tuple data type %q",
			ErrInvariantViolation, rel.columns[rel.keyIndex].Name, c.DataType)
	}
	key, err := strconv.ParseInt(string(c.Data), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: CO-5: key column %s: %w", ErrInvariantViolation, rel.columns[rel.keyIndex].Name, err)
	}
	return key, nil
}
