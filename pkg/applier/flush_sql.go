package applier

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/decode"
)

// split sorts an image's writable columns into the ones it carries a value
// for, in shadow column order, and the names it does not: a decoded marker,
// or a column the image never named. A decoded column the shadow does not
// hold is dropped, and the primary key column is never written from the
// image — Entry.Key is the key.
func (f *Flusher) split(e *Entry) (present []decode.Column, markers []string) {
	for _, name := range f.columns {
		if i := e.index(name); i >= 0 && e.Columns[i].Present {
			present = append(present, e.Columns[i])
			continue
		}
		markers = append(markers, name)
	}
	return present, markers
}

// upsertSQL writes the key and the given columns into relation, replacing
// the row's values for exactly those columns when the key is already held.
// The key is $1; the columns follow in order. A row with no column but the
// key is written by assigning the key to itself, which PostgreSQL accepts
// as an update that changes nothing.
func upsertSQL(relation, pk string, cols []decode.Column) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(relation)
	b.WriteString(" (")
	b.WriteString(columnList(pk, cols))
	b.WriteString(") VALUES (")
	b.WriteString(placeholders(1 + len(cols)))
	b.WriteString(") ON CONFLICT (")
	b.WriteString(pk)
	b.WriteString(") DO UPDATE SET ")
	if len(cols) == 0 {
		b.WriteString(pk)
		b.WriteString(" = EXCLUDED.")
		b.WriteString(pk)
	}
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		name := pgx.Identifier{c.Name}.Sanitize()
		b.WriteString(name)
		b.WriteString(" = EXCLUDED.")
		b.WriteString(name)
	}
	return b.String()
}

// updateSQL assigns the given columns on relation's row under key $1 and
// nothing else; the columns follow the key in order. It is the write for an
// image that still carries a marker, so the row has to exist (CO-8); with
// no column to assign it assigns the key to itself, so the row count still
// reports whether the row is there.
func updateSQL(relation, pk string, cols []decode.Column) string {
	var b strings.Builder
	b.WriteString("UPDATE ")
	b.WriteString(relation)
	b.WriteString(" SET ")
	if len(cols) == 0 {
		b.WriteString(pk)
		b.WriteString(" = $1")
	}
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pgx.Identifier{c.Name}.Sanitize())
		b.WriteString(" = $")
		b.WriteString(strconv.Itoa(i + 2))
	}
	b.WriteString(" WHERE ")
	b.WriteString(pk)
	b.WriteString(" = $1")
	return b.String()
}

// insertSQL writes a whole row into relation: the key is $1 and every
// writable column follows in order. It is the fallback's write, and it
// carries no conflict clause because every key in the batch was deleted
// first (D13).
func insertSQL(relation, pk string, cols []decode.Column) string {
	return "INSERT INTO " + relation + " (" + columnList(pk, cols) + ") VALUES (" + placeholders(1+len(cols)) + ")"
}

// deleteSQL removes relation's row under key $1.
func deleteSQL(relation, pk string) string {
	return "DELETE FROM " + relation + " WHERE " + pk + " = $1"
}

// deleteAllSQL removes relation's rows under every key in the bigint array
// $1.
func deleteAllSQL(relation, pk string) string {
	return "DELETE FROM " + relation + " WHERE " + pk + " = ANY($1::bigint[])"
}

// rowSQL reads the named columns of relation's row under key $1, each
// rendered as text, and last the server's WAL insert position as text. The
// position is taken inside the statement, after its snapshot: every change
// the read saw committed below it, so the buffer can tell when the stream
// has delivered everything the read knew about.
func rowSQL(relation, pk string, names []string) string {
	cols := make([]string, 0, len(names)+1)
	for _, name := range names {
		cols = append(cols, pgx.Identifier{name}.Sanitize()+"::text")
	}
	cols = append(cols, "pg_catalog.pg_current_wal_insert_lsn()::text")
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + relation + " WHERE " + pk + " = $1"
}

func (f *Flusher) upsertRow(ctx context.Context, tx pgx.Tx, key int64, cols []decode.Column) error {
	if _, err := tx.Exec(ctx, upsertSQL(f.shadowRelation(), f.pk(), cols), args(key, cols)...); err != nil {
		return fmt.Errorf("upsert key %d into %s: %w", key, f.shadowRelation(), err)
	}
	return nil
}

// updateRow writes the columns onto the row under key and reports whether a
// row was there to take them.
func (f *Flusher) updateRow(ctx context.Context, tx pgx.Tx, key int64, cols []decode.Column) (bool, error) {
	tag, err := tx.Exec(ctx, updateSQL(f.shadowRelation(), f.pk(), cols), args(key, cols)...)
	if err != nil {
		return false, fmt.Errorf("update key %d in %s: %w", key, f.shadowRelation(), err)
	}
	return tag.RowsAffected() == 1, nil
}

func (f *Flusher) insertRow(ctx context.Context, tx pgx.Tx, key int64, cols []decode.Column) error {
	if _, err := tx.Exec(ctx, insertSQL(f.shadowRelation(), f.pk(), cols), args(key, cols)...); err != nil {
		return fmt.Errorf("insert key %d into %s: %w", key, f.shadowRelation(), err)
	}
	return nil
}

func (f *Flusher) deleteRow(ctx context.Context, tx pgx.Tx, key int64) error {
	if _, err := tx.Exec(ctx, deleteSQL(f.shadowRelation(), f.pk()), key); err != nil {
		return fmt.Errorf("delete key %d from %s: %w", key, f.shadowRelation(), err)
	}
	return nil
}

func (f *Flusher) deleteRows(ctx context.Context, tx pgx.Tx, keys []int64) error {
	if _, err := tx.Exec(ctx, deleteAllSQL(f.shadowRelation(), f.pk()), keys); err != nil {
		return fmt.Errorf("delete %d keys from %s: %w", len(keys), f.shadowRelation(), err)
	}
	return nil
}

// args lays out a statement's parameters: the key first, then each
// column's value in order, so every builder above binds the same way.
func args(key int64, cols []decode.Column) []any {
	out := make([]any, 0, 1+len(cols))
	out = append(out, key)
	for _, c := range cols {
		out = append(out, c.Value)
	}
	return out
}

func (f *Flusher) shadowRelation() string {
	return pgx.Identifier{f.shadow.Schema(), f.shadow.ShadowTable()}.Sanitize()
}

func (f *Flusher) sourceRelation() string {
	return pgx.Identifier{f.shadow.Schema(), f.shadow.SourceTable()}.Sanitize()
}

func (f *Flusher) pk() string {
	return pgx.Identifier{f.target.PKColumn()}.Sanitize()
}

// columnList renders the key column followed by the given columns, quoted
// and comma-separated.
func columnList(pk string, cols []decode.Column) string {
	names := make([]string, 0, 1+len(cols))
	names = append(names, pk)
	for _, c := range cols {
		names = append(names, pgx.Identifier{c.Name}.Sanitize())
	}
	return strings.Join(names, ", ")
}

// placeholders renders $1 … $n.
func placeholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "$" + strconv.Itoa(i+1)
	}
	return strings.Join(parts, ", ")
}
