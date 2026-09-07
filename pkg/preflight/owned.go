package preflight

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OwnedRelationNames contains the relation names a table owns: the indexes
// backing its own primary-key, unique, and exclusion constraints, and the
// sequences owned by its columns. Each list is sorted and duplicate-free.
// A name is present whether the server invented it or the desired file
// stated it — a named constraint's index and an ALTER SEQUENCE ... OWNED BY
// sequence are owned just the same.
type OwnedRelationNames struct {
	ConstraintIndexes []string
	Sequences         []string
}

// LookupOwnedRelationNames reads the relation names owned by an ordinary or
// partitioned table so a caller can verify them after a create step. Schema
// must be explicit because the caller must inspect the exact schema covered
// by its earlier absence proof rather than resolve a possibly different
// search_path target. The read excludes standalone indexes, whose own create
// steps report duplicate-name SQLSTATEs; the table itself, whose name is
// covered by the caller's absence proof; and the indexes a foreign key
// borrows from its referenced table, which belong to that table.
func LookupOwnedRelationNames(ctx context.Context, pool *pgxpool.Pool, schema, table string) (OwnedRelationNames, error) {
	if schema == "" {
		return OwnedRelationNames{}, fmt.Errorf("look up owned relation names for %s: empty schema", table)
	}
	// A foreign key's conindid is the referenced table's index, so the
	// constraint arm selects by contype rather than by conindid: only
	// primary-key, unique, and exclusion constraints build an index on the
	// table itself. The sequence arm's deptype pair is the definition of
	// column ownership — 'a' for serial and OWNED BY, 'i' for identity.
	const q = `
		WITH target AS (
			SELECT c.oid, c.relkind::text AS relkind
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1
			  AND c.relname = $2
		)
		SELECT t.relkind, ARRAY(
			SELECT i.relname
			FROM pg_constraint c
			JOIN pg_class i ON i.oid = c.conindid
			WHERE c.conrelid = t.oid
			  AND c.contype IN ('p', 'u', 'x')
			ORDER BY i.relname
		), ARRAY(
			SELECT s.relname
			FROM pg_depend d
			JOIN pg_class s ON s.oid = d.objid
			WHERE d.refclassid = 'pg_class'::regclass
			  AND d.refobjid = t.oid
			  AND d.refobjsubid > 0
			  AND d.classid = 'pg_class'::regclass
			  AND d.deptype IN ('a', 'i')
			  AND s.relkind = 'S'
			ORDER BY s.relname
		)
		FROM target t`
	var relkind string
	var names OwnedRelationNames
	err := pool.QueryRow(ctx, q, schema, table).Scan(&relkind, &names.ConstraintIndexes, &names.Sequences)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnedRelationNames{}, fmt.Errorf("%w: %s", ErrTableNotFound, qualifiedName(schema, table))
	}
	if err != nil {
		return OwnedRelationNames{}, fmt.Errorf("look up owned relation names for %s: %w", qualifiedName(schema, table), err)
	}
	// relkind 'r' is an ordinary table, 'p' a partitioned parent; anything
	// else at the name is not a table this lookup can describe.
	if relkind != "r" && relkind != "p" {
		return OwnedRelationNames{}, fmt.Errorf("%w: %s has relkind %q", ErrNotTable, qualifiedName(schema, table), relkind)
	}
	return names, nil
}
