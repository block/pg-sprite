package applier

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/decode"
)

// shadowRowSQL reads the image's marker columns from the shadow row under
// key $1, each rendered as text, and locks the row for the rest of the
// transaction, so a delete of that row later in the same flush cannot have
// happened between the read and the write.
func (f *Flusher) shadowRowSQL(e *Entry) string {
	_, markers := f.split(e)
	return rowSQL(f.shadowRelation(), f.pk(), markers) + " FOR UPDATE"
}

// sourceRowSQL reads the image's marker columns from the live source row
// under key $1, each rendered as text. The source is not locked: the flush
// holds ACCESS SHARE on it and reads whatever version is current, which is
// the value the marker stood for or a later one that a later event will
// write again (D13).
func (f *Flusher) sourceRowSQL(e *Entry) string {
	_, markers := f.split(e)
	return rowSQL(f.sourceRelation(), f.pk(), markers)
}

// completeFrom runs a row read for the image's markers and writes what it
// finds into the image, each value as the text the server rendered — which
// the write binds back as text for the server to read with the column's
// own input function, so a value of any type makes the round trip — and
// NULL as NULL. It reports false, and changes nothing, when the row is
// absent.
func (f *Flusher) completeFrom(ctx context.Context, tx pgx.Tx, sql string, key int64, e *Entry) (bool, error) {
	_, markers := f.split(e)
	values := make([]*string, len(markers))
	dest := make([]any, len(markers))
	for i := range values {
		dest[i] = &values[i]
	}
	err := tx.QueryRow(ctx, sql, key).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("complete key %d from the row under key %d: %w", e.Key, key, err)
	}
	for i, name := range markers {
		col := decode.Column{Name: name, Present: true}
		if values[i] != nil {
			col.Value = *values[i]
		}
		e.overlay([]decode.Column{col})
	}
	return true, nil
}
