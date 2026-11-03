package schemadiff

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// ErrReferencedTableNotFound is returned when a desired file's foreign key
// references a table that does not exist in the target schema. The scratch
// build binds each foreign key to an empty copy of the live referenced
// table, so the referenced table must already exist.
var ErrReferencedTableNotFound = errors.New("foreign key references a table that does not exist in the target schema")

// ErrReferencedTableNotReadable is returned when the planning role lacks
// SELECT on a table a desired file's foreign key references. The scratch
// build copies the referenced table's shape with LIKE, which PostgreSQL
// allows only to a role that can read the source table.
var ErrReferencedTableNotReadable = errors.New("planning role lacks SELECT on a table a foreign key references")

// LIKE raises undefined_table when its source table does not exist, and
// insufficient_privilege when the role cannot read it.
const (
	sqlstateUndefinedTable        = "42P01"
	sqlstateInsufficientPrivilege = "42501"
)

// createReferencedStandIns creates, in the scratch schema, an empty copy of
// each live table the desired foreign keys reference, before the desired
// CREATE TABLE runs. Each foreign key then binds to a scratch relation of
// the same name, never to the live table: creating a foreign key locks the
// referenced table in SHARE ROW EXCLUSIVE mode, which would block writes to
// the live table until the scratch transaction rolls back. LIKE reads the
// live table under ACCESS SHARE only, the lock a plain SELECT takes.
// INCLUDING INDEXES carries the primary key and unique constraints a
// foreign key must match; the column types come with every LIKE. Because
// the stand-in has the referenced table's name and the scratch schema leads
// the search_path, the server prints the constraint definition unqualified,
// exactly as live introspection prints the live constraint.
//
// LIKE names each copied index after the stand-in table, not after the live
// index, so a copied name such as users_pkey could be one the desired file
// creates. Each stand-in index is renamed before the desired statements run
// to a name built from the scratch schema's random name, which a desired
// file cannot know in advance; a foreign key's printed definition names only
// the referenced table and columns, so the rename never changes it.
func createReferencedStandIns(ctx context.Context, tx pgx.Tx, scratch, schema string, tables []string) error {
	renamed := 0
	for _, table := range tables {
		sql := "CREATE TABLE " + pgx.Identifier{scratch, table}.Sanitize() +
			" (LIKE " + pgx.Identifier{schema, table}.Sanitize() + " INCLUDING INDEXES)"
		if _, err := tx.Exec(ctx, sql); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				switch pgErr.Code {
				case sqlstateUndefinedTable:
					return fmt.Errorf("%w: %s.%s", ErrReferencedTableNotFound, schema, table)
				case sqlstateInsufficientPrivilege:
					return fmt.Errorf("%w: %s.%s: %w", ErrReferencedTableNotReadable, schema, table, err)
				}
			}
			return fmt.Errorf("create scratch stand-in for referenced table %s.%s: %w", schema, table, err)
		}
		n, err := renameStandInIndexes(ctx, tx, scratch, table, renamed)
		if err != nil {
			return err
		}
		renamed += n
	}
	return nil
}

// renameStandInIndexes renames the stand-in table's indexes to names that
// begin with the scratch schema's name and are numbered from next, and
// returns how many it renamed. Renaming a constraint's index renames the
// constraint with it.
func renameStandInIndexes(ctx context.Context, tx pgx.Tx, scratch, table string, next int) (int, error) {
	// INV: CO-9 — the read runs under the scratch build's path, which lists
	// the target schema, so every relation and operator is qualified.
	rows, err := tx.Query(ctx, `
		SELECT idx.relname
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class idx ON idx.oid OPERATOR(pg_catalog.=) i.indexrelid
		JOIN pg_catalog.pg_class rel ON rel.oid OPERATOR(pg_catalog.=) i.indrelid
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) rel.relnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND rel.relname OPERATOR(pg_catalog.=) $2
		ORDER BY idx.relname`, scratch, table)
	if err != nil {
		return 0, fmt.Errorf("read scratch stand-in indexes of %s: %w", table, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("read scratch stand-in indexes of %s: %w", table, err)
	}
	for i, name := range names {
		sql := "ALTER INDEX " + pgx.Identifier{scratch, name}.Sanitize() +
			" RENAME TO " + pgx.Identifier{fmt.Sprintf("%s_index_%d", scratch, next+i)}.Sanitize()
		if _, err := tx.Exec(ctx, sql); err != nil {
			return 0, fmt.Errorf("rename scratch stand-in index %s of %s: %w", name, table, err)
		}
	}
	return len(names), nil
}

// confirmForeignKeysBindScratch fails closed unless every foreign key the
// desired table carries references a relation in the scratch schema. The
// stand-ins make that true by construction; this proves it, so a reference
// the parse boundary failed to list surfaces as an invariant violation
// rather than as a comparison against a live table's binding.
func confirmForeignKeysBindScratch(ctx context.Context, tx pgx.Tx, scratch, table string) error {
	var escaped []string
	// INV: CO-9 — the read runs under the scratch build's path, which lists
	// the target schema, so every relation and operator is qualified.
	rows, err := tx.Query(ctx, `
		SELECT con.conname
		FROM pg_catalog.pg_constraint con
		JOIN pg_catalog.pg_class rel ON rel.oid OPERATOR(pg_catalog.=) con.conrelid
		JOIN pg_catalog.pg_namespace reln ON reln.oid OPERATOR(pg_catalog.=) rel.relnamespace
		JOIN pg_catalog.pg_class ref ON ref.oid OPERATOR(pg_catalog.=) con.confrelid
		JOIN pg_catalog.pg_namespace refn ON refn.oid OPERATOR(pg_catalog.=) ref.relnamespace
		WHERE con.contype OPERATOR(pg_catalog.=) 'f'
		      AND reln.nspname OPERATOR(pg_catalog.=) $1 AND rel.relname OPERATOR(pg_catalog.=) $2
		      AND refn.nspname OPERATOR(pg_catalog.<>) $1
		ORDER BY con.conname`, scratch, table)
	if err != nil {
		return fmt.Errorf("read scratch foreign keys of %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan scratch foreign key of %s: %w", table, err)
		}
		escaped = append(escaped, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read scratch foreign keys of %s: %w", table, err)
	}
	if len(escaped) > 0 {
		return fmt.Errorf("%w: foreign keys %q on desired table %q reference a relation outside the scratch schema",
			dbconn.ErrInvariantViolation, escaped, table)
	}
	return nil
}
