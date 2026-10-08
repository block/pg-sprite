package applier

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/decode"
)

// shadowRowSQL reads the image's marker columns from the shadow row under
// key $1, each rendered as text, with the WAL position of the read, and
// locks the row for the rest of the transaction, so a delete of that row
// later in the same flush cannot have happened between the read and the
// write.
func (f *Flusher) shadowRowSQL(e *Entry) string {
	_, markers := f.split(e)
	return rowSQL(f.shadowRelation(), f.pk(), markers) + " FOR UPDATE"
}

// sourceRowSQL reads the image's marker columns from the live source row
// under key $1, each rendered as text, with the WAL position of the read.
// The source is not locked: the flush holds ACCESS SHARE on it and reads
// whatever version is current, and the read's position is what tells the
// buffer when the stream has caught up with that version (D13).
func (f *Flusher) sourceRowSQL(e *Entry) string {
	_, markers := f.split(e)
	return rowSQL(f.sourceRelation(), f.pk(), markers)
}

// completion is what one row read found for an image's markers.
type completion struct {
	// columns carries a present value for every marker column, each as the
	// text the server rendered — which the write binds back as text for the
	// server to read with the column's own input function, so a value of
	// any type makes the round trip — and NULL as NULL.
	columns []decode.Column
	// readLSN is the WAL insert position at the read. Every change the read
	// saw had committed below it.
	readLSN decode.LSN
}

// completeFrom runs a row read for the image's markers and returns what it
// found. It reports false when the row is absent.
func (f *Flusher) completeFrom(ctx context.Context, tx pgx.Tx, sql string, key int64, e *Entry) (completion, bool, error) {
	_, markers := f.split(e)
	values := make([]*string, len(markers))
	dest := make([]any, 0, len(markers)+1)
	for i := range values {
		dest = append(dest, &values[i])
	}
	var readAt string
	dest = append(dest, &readAt)
	err := tx.QueryRow(ctx, sql, key).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return completion{}, false, nil
	}
	if err != nil {
		return completion{}, false, fmt.Errorf("complete key %d from the row under key %d: %w", e.Key, key, err)
	}
	readLSN, err := decode.ParseLSN(readAt)
	if err != nil {
		return completion{}, false, fmt.Errorf("complete key %d from the row under key %d: read position: %w", e.Key, key, err)
	}
	c := completion{columns: make([]decode.Column, len(markers)), readLSN: readLSN}
	for i, name := range markers {
		c.columns[i] = decode.Column{Name: name, Present: true}
		if values[i] != nil {
			c.columns[i].Value = *values[i]
		}
	}
	return c, true, nil
}
