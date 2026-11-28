package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The column check of the online attach. PostgreSQL refuses an ATTACH
// (tablecmds.c, ATExecAttachPartition and MergeAttributesIntoExisting) whose
// table lacks a column of the parent, has a column the parent lacks, or has a
// column of another type, type modifier, or collation than the parent's. It
// refuses a plain column where the parent's is generated, and, from
// PostgreSQL 16, a generated column where the parent's is plain or generated
// of the other kind; before 16, a generated column whose expression differs
// from the parent's. From PostgreSQL 17 it refuses a standalone table with an
// identity column. It does so only at the ATTACH, after every proof, CHECK,
// index, and foreign key the flow gives the table, so the flow reads the same
// rules before it changes anything.

// attachGeneratedOnlyParentMajor is the first server major that refuses a
// generated column of the table where the parent's is plain, and that no
// longer compares the generation expressions of two generated columns.
const attachGeneratedOnlyParentMajor = 16

// attachIdentityRefusedMajor is the first server major that refuses to
// attach a table with an identity column.
const attachIdentityRefusedMajor = 17

// attachColumnShape is one column as the attach compares it.
type attachColumnShape struct {
	// typ is the type with its modifier, as format_type prints it.
	typ       string
	collation uint32
	// generated is pg_attribute.attgenerated: empty for a plain column, s
	// for a stored generated one, v for a virtual one.
	generated string
	// expression is the generation expression as pg_get_expr prints it, not
	// pretty-printed, empty for a plain column.
	expression string
	// identity is pg_attribute.attidentity: empty, or a for ALWAYS, d for
	// BY DEFAULT.
	identity string
}

// attachColumnsSQL reads a table's live columns by name. Every relation and
// function is pg_catalog-qualified (CO-9).
const attachColumnsSQL = `SELECT a.attname::pg_catalog.text,
		pg_catalog.format_type(a.atttypid, a.atttypmod),
		a.attcollation::pg_catalog.oid,
		a.attgenerated::pg_catalog.text,
		COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid, false), ''),
		a.attidentity::pg_catalog.text
	FROM pg_catalog.pg_attribute a
	LEFT JOIN pg_catalog.pg_attrdef d
	       ON d.adrelid OPERATOR(pg_catalog.=) a.attrelid
	      AND d.adnum OPERATOR(pg_catalog.=) a.attnum
	      AND a.attgenerated OPERATOR(pg_catalog.<>) ''
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
	_, err = pgx.ForEachRow(rows, []any{&name, &c.typ, &c.collation, &c.generated, &c.expression, &c.identity}, func() error {
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
	var versionNum int
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4`).Scan(&versionNum); err != nil {
		return fmt.Errorf("read server_version_num: %w", err)
	}
	major := versionNum / 10000
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
		differences = append(differences, attachColumnDifferences(name, c, parent[name], major)...)
	}
	for _, name := range tableNames {
		if _, ok := parent[name]; !ok {
			differences = append(differences, fmt.Sprintf("its column %s is not a column of the parent", pgx.Identifier{name}.Sanitize()))
		}
		if attachRefusesIdentity(columns[name], table, major) {
			differences = append(differences, fmt.Sprintf("its column %s is an identity column", pgx.Identifier{name}.Sanitize()))
		}
	}
	if len(differences) > 0 {
		return fmt.Errorf("%w: %s does not have the parent's columns, which the ATTACH requires: %s",
			ErrAttachPartitionTable, qualifiedName(table.schema, table.name), strings.Join(differences, "; "))
	}
	return nil
}

// attachColumnDifferences names how the table's column c differs from the
// parent's column want in a way the ATTACH refuses on a server of major.
func attachColumnDifferences(name string, c, want attachColumnShape, major int) []string {
	quoted := pgx.Identifier{name}.Sanitize()
	var differences []string
	if c.typ != want.typ {
		differences = append(differences, fmt.Sprintf("its column %s is %s, and the parent's is %s", quoted, c.typ, want.typ))
	}
	if c.collation != want.collation {
		differences = append(differences, fmt.Sprintf("its column %s has collation %d, and the parent's has %d", quoted, c.collation, want.collation))
	}
	switch {
	case want.generated != "" && c.generated == "":
		differences = append(differences, fmt.Sprintf("its column %s is not generated, and the parent's is", quoted))
	case want.generated == "" && c.generated != "":
		if major >= attachGeneratedOnlyParentMajor {
			differences = append(differences, fmt.Sprintf("its column %s is generated, and the parent's is not", quoted))
		}
	case want.generated != c.generated:
		differences = append(differences, fmt.Sprintf("its column %s is generated of kind %q, and the parent's of kind %q", quoted, c.generated, want.generated))
	case want.generated != "" && c.expression != want.expression:
		if major < attachGeneratedOnlyParentMajor {
			differences = append(differences, fmt.Sprintf("its column %s is generated as %s, and the parent's as %s", quoted, c.expression, want.expression))
		}
	}
	return differences
}

// attachRefusesIdentity says whether the ATTACH refuses the table for its
// column c being an identity column. A partition carries the parent's
// identity, so only a standalone table's is refused.
func attachRefusesIdentity(c attachColumnShape, table relationFacts, major int) bool {
	if major < attachIdentityRefusedMajor || table.isPartition {
		return false
	}
	return c.identity != ""
}
