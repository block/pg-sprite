package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// OwnedSequence is one sequence a source column owns through OWNED BY — a
// serial column's, or one the user attached by hand. LIKE … INCLUDING
// DEFAULTS makes the shadow column draw from the same sequence, which keeps
// its name; cutover re-owns it to the live table so it survives the old
// table's drop (D5). Identity sequences are not among these: they depend on
// their column internally and are handed off through IdentityColumn.
type OwnedSequence struct {
	// Column is the owning source column.
	Column string `json:"column"`
	// SequenceSchema is the sequence's schema.
	SequenceSchema string `json:"sequence_schema"`
	// SequenceName is the sequence's unqualified name.
	SequenceName string `json:"sequence_name"`
}

// readOwnedSequences lists the sequences the table's columns own through
// the auto (deptype 'a') pg_depend edge OWNED BY records, in column order.
func readOwnedSequences(ctx context.Context, tx pgx.Tx, oid uint32) ([]OwnedSequence, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.attname, sn.nspname, s.relname
		FROM pg_attribute a
		JOIN pg_depend d
		  ON d.refclassid = 'pg_class'::regclass AND d.refobjid = a.attrelid AND d.refobjsubid = a.attnum
		 AND d.classid = 'pg_class'::regclass AND d.deptype = 'a'
		JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
		JOIN pg_namespace sn ON sn.oid = s.relnamespace
		WHERE a.attrelid = $1 AND NOT a.attisdropped
		ORDER BY a.attnum, sn.nspname, s.relname`, oid)
	if err != nil {
		return nil, fmt.Errorf("read owned sequences: %w", err)
	}
	sequences, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OwnedSequence, error) {
		var s OwnedSequence
		err := row.Scan(&s.Column, &s.SequenceSchema, &s.SequenceName)
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("read owned sequences: %w", err)
	}
	return sequences, nil
}
