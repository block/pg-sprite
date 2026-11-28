package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The column check of the online attach. PostgreSQL refuses an ATTACH whose
// table lacks a column of the parent, has a column the parent lacks, or has
// a column of another type, type modifier, or collation than the parent's.
// It does so only at the ATTACH, after every proof, CHECK, index, and
// foreign key the flow gives the table, so the flow reads the same four
// rules before it changes anything.

// attachColumnShape is one column as the attach compares it.
type attachColumnShape struct {
	// typ is the type with its modifier, as format_type prints it.
	typ       string
	collation uint32
}

// attachColumnsSQL reads a table's live columns by name. Every relation and
// function is pg_catalog-qualified (CO-9).
const attachColumnsSQL = `SELECT a.attname::pg_catalog.text,
		pg_catalog.format_type(a.atttypid, a.atttypmod),
		a.attcollation::pg_catalog.oid
	FROM pg_catalog.pg_attribute a
	WHERE a.attrelid OPERATOR(pg_catalog.=) $1
	  AND a.attnum OPERATOR(pg_catalog.>) 0
	  AND NOT a.attisdropped
	ORDER BY a.attname`

// readAttachColumns reads the table's live columns, in name order.
func readAttachColumns(ctx context.Context, tx pgx.Tx, oid uint32) ([]string, map[string]attachColumnShape, error) {
	rows, err := tx.Query(ctx, attachColumnsSQL, oid)
	if err != nil {
		return nil, nil, fmt.Errorf("read the columns of table %d: %w", oid, err)
	}
	var names []string
	columns := map[string]attachColumnShape{}
	var name string
	var c attachColumnShape
	_, err = pgx.ForEachRow(rows, []any{&name, &c.typ, &c.collation}, func() error {
		names = append(names, name)
		columns[name] = c
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read the columns of table %d: %w", oid, err)
	}
	return names, columns, nil
}

// requireAttachColumns refuses a table whose columns the ATTACH would
// refuse, naming every difference.
func requireAttachColumns(ctx context.Context, tx pgx.Tx, parentOID uint32, table relationFacts) error {
	parentNames, parent, err := readAttachColumns(ctx, tx, parentOID)
	if err != nil {
		return err
	}
	tableNames, columns, err := readAttachColumns(ctx, tx, table.oid)
	if err != nil {
		return err
	}
	var differences []string
	for _, name := range parentNames {
		c, ok := columns[name]
		if !ok {
			differences = append(differences, fmt.Sprintf("it lacks the parent's column %s", pgx.Identifier{name}.Sanitize()))
			continue
		}
		want := parent[name]
		if c.typ != want.typ {
			differences = append(differences, fmt.Sprintf("its column %s is %s, and the parent's is %s", pgx.Identifier{name}.Sanitize(), c.typ, want.typ))
		}
		if c.collation != want.collation {
			differences = append(differences, fmt.Sprintf("its column %s has collation %d, and the parent's has %d", pgx.Identifier{name}.Sanitize(), c.collation, want.collation))
		}
	}
	for _, name := range tableNames {
		if _, ok := parent[name]; !ok {
			differences = append(differences, fmt.Sprintf("its column %s is not a column of the parent", pgx.Identifier{name}.Sanitize()))
		}
	}
	if len(differences) > 0 {
		return fmt.Errorf("%w: %s does not have the parent's columns, which the ATTACH requires: %s",
			ErrAttachPartitionTable, qualifiedName(table.schema, table.name), strings.Join(differences, "; "))
	}
	return nil
}
