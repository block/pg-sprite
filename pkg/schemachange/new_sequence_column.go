package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// sequenceOwnedColumn is one shadow column that owns a sequence after the
// gated statement ran on the shadow: an identity column, whose sequence
// depends on it internally, or a serial-style column whose sequence is
// OWNED BY it.
type sequenceOwnedColumn struct {
	column   string
	identity bool
}

// readSequenceOwnedColumns lists the shadow's columns that declare an
// identity or own a sequence, in attribute order. The shadow is cloned
// LIKE … EXCLUDING IDENTITY, a source identity column is handed off as a
// plain column with DEFAULT nextval(<source sequence>), and a source
// serial column's default is copied without the OWNED BY edge, so before
// the gated statement no shadow column declares an identity or owns a
// sequence; every one this read returns was declared by the statement.
func readSequenceOwnedColumns(ctx context.Context, tx pgx.Tx, shadowOID uint32) ([]sequenceOwnedColumn, error) {
	// INV: CO-9 — every relation, operator, and type is pg_catalog-qualified,
	// so a planted oid = regclass operator cannot hide a column's sequence
	// edge from the check.
	rows, err := tx.Query(ctx, `
		SELECT a.attname, a.attidentity OPERATOR(pg_catalog.<>) ''
		FROM pg_catalog.pg_attribute a
		WHERE a.attrelid OPERATOR(pg_catalog.=) $1
		  AND a.attnum OPERATOR(pg_catalog.>) 0
		  AND NOT a.attisdropped
		  AND (a.attidentity OPERATOR(pg_catalog.<>) ''
		       OR EXISTS (
		           SELECT 1
		           FROM pg_catalog.pg_depend d
		           JOIN pg_catalog.pg_class s
		             ON s.oid OPERATOR(pg_catalog.=) d.objid
		            AND s.relkind OPERATOR(pg_catalog.=) 'S'
		           WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		             AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		             AND d.refobjid OPERATOR(pg_catalog.=) a.attrelid
		             AND d.refobjsubid OPERATOR(pg_catalog.=) a.attnum
		             AND (d.deptype OPERATOR(pg_catalog.=) 'a' OR d.deptype OPERATOR(pg_catalog.=) 'i')))
		ORDER BY a.attnum`, shadowOID)
	if err != nil {
		return nil, fmt.Errorf("read sequence-owning columns: %w", err)
	}
	owned, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (sequenceOwnedColumn, error) {
		var c sequenceOwnedColumn
		err := row.Scan(&c.column, &c.identity)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read sequence-owning columns: %w", err)
	}
	return owned, nil
}

// refuseNewSequenceColumns refuses a gated statement that gives the shadow
// an identity or sequence-owned column the source does not have: an added
// identity column, an added serial column, or a kept column the statement
// made an identity. The swap carries the source's sequences and identity
// columns to the live table and proves the live table has exactly those;
// a sequence the statement minted on the shadow has no place in that
// handoff, so a run carrying one would copy every row and then stop at
// the swap. The build refuses it instead, inside its own transaction,
// before any shadow, fence, or checkpoint row exists. owned is the
// shadow's readSequenceOwnedColumns after the statement ran; the message
// names the first such column in attribute order.
func refuseNewSequenceColumns(schema, table string, owned []sequenceOwnedColumn) error {
	if len(owned) == 0 {
		return nil
	}
	// INV: ST-6
	c := owned[0]
	if c.identity {
		return refuse(CauseStatementNewSequence, nil, "the statement for %s.%s declares %s as an identity column, which the swap cannot carry to the live table; declare it a plain column here and add the identity in a later schema change", schema, table, c.column)
	}
	return refuse(CauseStatementNewSequence, nil, "the statement for %s.%s gives %s a sequence of its own, which the swap cannot carry to the live table; declare it a plain column here and attach the sequence in a later schema change", schema, table, c.column)
}
