package schemadiff

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrTableNotFound is returned when the target table does not exist in the
// requested schema.
var ErrTableNotFound = errors.New("table not found")

// ErrNotTable is returned when the target exists but is not an ordinary or
// partitioned table (e.g. a view or foreign table).
var ErrNotTable = errors.New("not an ordinary or partitioned table")

// Introspect reads the live table schema.table into the canonical model. It
// runs inside a read-only transaction whose search_path is set to the target
// schema (then public), so the server's decompilers print definitions
// unqualified — directly comparable with a desired-state model introspected
// the same way.
func Introspect(ctx context.Context, db *pgxpool.Pool, schema, table string) (Model, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return Model{}, fmt.Errorf("begin introspection: %w", err)
	}
	defer func() {
		// Redundant safety closer: the transaction is read-only and always
		// rolled back below; this only covers early error returns.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	m, err := introspectInTx(ctx, tx, schema, table)
	if err != nil {
		return Model{}, err
	}
	if err := tx.Rollback(ctx); err != nil {
		return Model{}, fmt.Errorf("end introspection: %w", err)
	}
	return m, nil
}

// introspectInTx introspects schema.table inside an open transaction. It
// sets the transaction-local search_path so decompiled definitions print
// unqualified, resolves the relation by explicit qualification (never via
// search_path), and reads columns, constraints, and indexes.
func introspectInTx(ctx context.Context, tx pgx.Tx, schema, table string) (Model, error) {
	// search_path cannot use bind parameters; identifiers are sanitized.
	setPath := "SET LOCAL search_path = " + pgx.Identifier{schema}.Sanitize() + ", public"
	if _, err := tx.Exec(ctx, setPath); err != nil {
		return Model{}, fmt.Errorf("set introspection search_path: %w", err)
	}

	var oid uint32
	var relkind string
	err := tx.QueryRow(ctx, `
		SELECT c.oid, c.relkind::text
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, table).Scan(&oid, &relkind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Model{}, fmt.Errorf("%s.%s: %w", schema, table, ErrTableNotFound)
	}
	if err != nil {
		return Model{}, fmt.Errorf("resolve table %s.%s: %w", schema, table, err)
	}
	if relkind != "r" && relkind != "p" {
		return Model{}, fmt.Errorf("%s.%s has relkind %q: %w", schema, table, relkind, ErrNotTable)
	}

	m := Model{Table: table}
	if m.Columns, err = introspectColumns(ctx, tx, oid); err != nil {
		return Model{}, fmt.Errorf("introspect columns of %s.%s: %w", schema, table, err)
	}
	if m.Constraints, err = introspectConstraints(ctx, tx, oid); err != nil {
		return Model{}, fmt.Errorf("introspect constraints of %s.%s: %w", schema, table, err)
	}
	if m.Indexes, err = introspectIndexes(ctx, tx, oid); err != nil {
		return Model{}, fmt.Errorf("introspect indexes of %s.%s: %w", schema, table, err)
	}
	return m, nil
}

// introspectColumns reads the canonical column list: server-formatted types
// and server-decompiled default/generation expressions, in attribute order.
func introspectColumns(ctx context.Context, tx pgx.Tx, oid uint32) ([]Column, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
		       a.attidentity::text,
		       a.attgenerated::text
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, fmt.Errorf("query columns: %w", err)
	}
	defer rows.Close()
	var cols []Column
	for rows.Next() {
		var c Column
		var identity, generated string
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &identity, &generated); err != nil {
			return nil, fmt.Errorf("scan column: %w", err)
		}
		c.Identity = Identity(identity)
		c.Generated = generated == "s"
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	return cols, nil
}

// introspectConstraints reads the table's own constraints (primary key,
// unique, check, foreign key, exclusion) as server-decompiled definitions.
// NOT NULL is modeled on the column (pg_attribute.attnotnull), so the PG 18
// pg_constraint rows for NOT NULL are deliberately excluded to keep the
// model identical across supported majors.
func introspectConstraints(ctx context.Context, tx pgx.Tx, oid uint32) ([]Constraint, error) {
	rows, err := tx.Query(ctx, `
		SELECT conname, pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = $1 AND contype IN ('p','u','c','f','x') AND conislocal
		ORDER BY conname`, oid)
	if err != nil {
		return nil, fmt.Errorf("query constraints: %w", err)
	}
	defer rows.Close()
	var cons []Constraint
	for rows.Next() {
		var c Constraint
		if err := rows.Scan(&c.Name, &c.Def); err != nil {
			return nil, fmt.Errorf("scan constraint: %w", err)
		}
		cons = append(cons, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read constraints: %w", err)
	}
	return cons, nil
}

// introspectIndexes reads the non-constraint indexes as server-decompiled
// CREATE INDEX statements. Constraint-backed indexes (primary key, unique
// constraint, exclusion) are represented by their constraint instead.
// pg_get_indexdef always schema-qualifies the ON clause, so the
// qualification is stripped to keep the model schema-relative and
// comparable between the live and scratch sides.
func introspectIndexes(ctx context.Context, tx pgx.Tx, oid uint32) ([]Index, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.relname, pg_get_indexdef(i.indexrelid)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = $1
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.indexrelid)
		ORDER BY c.relname`, oid)
	if err != nil {
		return nil, fmt.Errorf("query indexes: %w", err)
	}
	defer rows.Close()
	var idxs []Index
	for rows.Next() {
		var ix Index
		if err := rows.Scan(&ix.Name, &ix.Def); err != nil {
			return nil, fmt.Errorf("scan index: %w", err)
		}
		if ix.Def, err = statement.Qualify(ix.Def, ""); err != nil {
			return nil, fmt.Errorf("unqualify index %s: %w", ix.Name, err)
		}
		idxs = append(idxs, ix)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	return idxs, nil
}
