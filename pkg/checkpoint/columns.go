package checkpoint

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// column is one column of the checkpoint table: its name and the
// definition that both CREATE TABLE and ADD COLUMN spell it with. Names are
// the engine's own constants, never a caller's.
type column struct {
	name       string
	definition string
}

// tableColumns is the checkpoint table's shape, in column order. It is the
// one place the shape is written down: createTableSQL renders it, Ensure
// adds any column of it that an older table lacks, and the table's own
// columns are judged against it. A column added here after the table has
// shipped must be nullable, since Ensure adds it to a table that may
// already hold rows.
func tableColumns() []column {
	return []column{
		{"schema_name", "text NOT NULL"},
		{"table_name", "text NOT NULL"},
		{"format_version", "integer NOT NULL"},
		{"shadow_table", "text NOT NULL"},
		{"slot_name", "text NOT NULL"},
		{"publication_name", "text NOT NULL"},
		{"watermark", "bigint"},
		{"last_applied_lsn", "pg_lsn NOT NULL"},
		{"source_fingerprint", "text NOT NULL"},
		{"target_fingerprint", "text NOT NULL"},
		{"phase", "text NOT NULL"},
		{"updated_at", "timestamptz NOT NULL"},
		{"shadow_proof", "jsonb"},
	}
}

// tableConstraintSQL is the table's one constraint: one row per target.
const tableConstraintSQL = "PRIMARY KEY (schema_name, table_name)"

// createTableSQL renders the CREATE TABLE for the whole shape. watermark is
// NULL while nothing has landed; phase holds the stable Phase name so an
// operator reading the row during an incident sees "copying", not a
// number; shadow_proof is NULL until the run has a shadow to prove.
func createTableSQL() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + tableIdent + " (\n")
	for _, c := range tableColumns() {
		fmt.Fprintf(&b, "\t%s %s,\n", c.name, c.definition)
	}
	b.WriteString("\t" + tableConstraintSQL + "\n)")
	return b.String()
}

// addColumnSQL renders the ALTER TABLE that brings a table lacking c up to
// the shape.
func addColumnSQL(c column) string {
	return "ALTER TABLE " + tableIdent + " ADD COLUMN " + c.name + " " + c.definition
}

// columnsSQL lists the live columns of the checkpoint table in order.
const columnsSQL = `
SELECT a.attname
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`

// tableShape is how an existing checkpoint table compares with the shape
// this engine knows: the known columns it lacks, and the columns it has
// that this engine does not know.
type tableShape struct {
	missing []column
	unknown []string
}

// inspectTableShape reads the table's columns and compares them, by name,
// with tableColumns.
func inspectTableShape(ctx context.Context, tx pgx.Tx) (tableShape, error) {
	rows, err := tx.Query(ctx, columnsSQL, SchemaName, TableName)
	if err != nil {
		return tableShape{}, fmt.Errorf("read columns of %s: %w", tableIdent, err)
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return tableShape{}, fmt.Errorf("read columns of %s: %w", tableIdent, err)
	}
	known := tableColumns()
	var shape tableShape
	for _, c := range known {
		if !slices.Contains(live, c.name) {
			shape.missing = append(shape.missing, c)
		}
	}
	for _, name := range live {
		if !slices.ContainsFunc(known, func(c column) bool { return c.name == name }) {
			shape.unknown = append(shape.unknown, name)
		}
	}
	return shape, nil
}

// evolveTable brings an existing checkpoint table up to this engine's
// shape, additively: each known column the table lacks is added. A table
// with columns this engine does not know — a newer engine's, or one an
// operator added by hand — is refused with a *TableNewerError and no DDL
// runs, since this engine cannot preserve the meaning of a column it has
// never heard of; the table is left exactly as it was. It runs inside
// Ensure's locked transaction, so two engines evolving one table
// serialize. Each ADD COLUMN takes ACCESS EXCLUSIVE on the table and waits
// at most the session's lock_timeout for the other engines' writes to
// clear; a lock timeout is returned as the server reported it, the
// transaction rolls back, and Ensure is safe to call again.
func evolveTable(ctx context.Context, tx pgx.Tx) error {
	// INV: ST-10
	shape, err := inspectTableShape(ctx, tx)
	if err != nil {
		return err
	}
	if len(shape.unknown) > 0 {
		return &TableNewerError{Unknown: shape.unknown}
	}
	for _, c := range shape.missing {
		if _, err := tx.Exec(ctx, addColumnSQL(c)); err != nil {
			return fmt.Errorf("add column %s to %s: %w", c.name, tableIdent, err)
		}
	}
	return nil
}
