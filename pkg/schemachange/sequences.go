package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
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
	// INV: CO-9 — every relation, operator, and type is pg_catalog-qualified,
	// so the answer does not depend on which search_path the transaction
	// holds when the read runs.
	rows, err := tx.Query(ctx, `
		SELECT a.attname, sn.nspname, s.relname
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_depend d
		  ON d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		 AND d.refobjid OPERATOR(pg_catalog.=) a.attrelid
		 AND d.refobjsubid OPERATOR(pg_catalog.=) a.attnum
		 AND d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		 AND d.deptype OPERATOR(pg_catalog.=) 'a'
		JOIN pg_catalog.pg_class s
		  ON s.oid OPERATOR(pg_catalog.=) d.objid AND s.relkind OPERATOR(pg_catalog.=) 'S'
		JOIN pg_catalog.pg_namespace sn ON sn.oid OPERATOR(pg_catalog.=) s.relnamespace
		WHERE a.attrelid OPERATOR(pg_catalog.=) $1 AND NOT a.attisdropped
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

// keptSequences keeps the owned sequences whose owning column the gated
// statement kept on the shadow. A sequence owned by a dropped column has no
// column on the live table to be re-owned to; it goes with the old table,
// as the server's own DROP COLUMN would have taken it.
func keptSequences(sequences []OwnedSequence, shadow schemadiff.Model) []OwnedSequence {
	onShadow := make(map[string]bool, len(shadow.Columns))
	for _, c := range shadow.Columns {
		onShadow[c.Name] = true
	}
	kept := make([]OwnedSequence, 0, len(sequences))
	for _, s := range sequences {
		if onShadow[s.Column] {
			kept = append(kept, s)
		}
	}
	return kept
}
