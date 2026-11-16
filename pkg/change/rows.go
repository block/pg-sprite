package change

import (
	"errors"
	"fmt"
	"slices"

	"github.com/block/pg-sprite/pkg/decode"
)

// ErrInvariantViolation marks an internal contract the source broke or was
// handed broken; it always aborts the stream.
var ErrInvariantViolation = decode.ErrInvariantViolation

// ErrIncompleteImage is returned when an UPDATE carries no value for a
// column — the server left an out-of-line value unchanged and did not send
// it — so the whole row image a subscription applies cannot be built from
// the change.
var ErrIncompleteImage = errors.New("change: an UPDATE left an out-of-line value unsent; the row image is incomplete")

// ErrImmutableColumnChanged is returned when an UPDATE changes the column a
// subscription declared immutable.
var ErrImmutableColumnChanged = errors.New("change: an UPDATE changed a column the subscription declares immutable")

// rowChange is one HasChanged call: the key, the whole image (nil for a
// delete), and whether the row was deleted.
type rowChange struct {
	key     []any
	image   []any
	deleted bool
}

// rowChanges turns one decoded change into the HasChanged calls that apply
// it. An UPDATE that moved the key is a deletion of the old key and an image
// under the new one. The key is the decoded primary-key value; the image is
// each column's value as the stream carried it — pgoutput's text form, or
// nil for NULL — in the table's column order.
func rowChanges(ev decode.ChangeEvent, tbl *TableInfo, immutableKey bool) ([]rowChange, error) {
	key := []any{ev.Key}
	switch ev.Kind {
	case decode.Insert:
		image, err := rowImage(ev, tbl)
		if err != nil {
			return nil, err
		}
		return []rowChange{{key: key, image: image}}, nil
	case decode.Update:
		image, err := rowImage(ev, tbl)
		if err != nil {
			return nil, err
		}
		if ev.OldKey == nil {
			return []rowChange{{key: key, image: image}}, nil
		}
		if immutableKey {
			return nil, fmt.Errorf("%w: %s.%s key %d moved to %d at %s",
				ErrImmutableColumnChanged, tbl.SchemaName, tbl.TableName, *ev.OldKey, ev.Key, ev.LSN)
		}
		return []rowChange{{key: []any{*ev.OldKey}, deleted: true}, {key: key, image: image}}, nil
	case decode.Delete:
		return []rowChange{{key: key, deleted: true}}, nil
	default:
		return nil, fmt.Errorf("%w: %s.%s at %s: change kind %s",
			ErrInvariantViolation, tbl.SchemaName, tbl.TableName, ev.LSN, ev.Kind)
	}
}

// rowImage is the change's whole image in the table's column order. The
// stream sends columns in the relation's order, which must be the table's.
func rowImage(ev decode.ChangeEvent, tbl *TableInfo) ([]any, error) {
	names := make([]string, len(ev.Columns))
	for i, c := range ev.Columns {
		names[i] = c.Name()
	}
	if !slices.Equal(names, tbl.Columns) {
		return nil, fmt.Errorf("%w: %s.%s at %s: the stream's columns %v are not the subscription's %v",
			ErrInvariantViolation, tbl.SchemaName, tbl.TableName, ev.LSN, names, tbl.Columns)
	}
	image := make([]any, len(ev.Columns))
	for i, c := range ev.Columns {
		value, present := c.Get()
		// INV: CO-8 — an unchanged out-of-line value is absent, not NULL;
		// the image is refused rather than carrying a guess for it.
		if !present {
			return nil, fmt.Errorf("%w: %s.%s column %s, key %d, at %s",
				ErrIncompleteImage, tbl.SchemaName, tbl.TableName, c.Name(), ev.Key, ev.LSN)
		}
		image[i] = value
	}
	return image, nil
}
