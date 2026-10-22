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
	//
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so a planted overload on the session's
	// search_path cannot hide an owned name from the verification.
	const q = `
		WITH target AS (
			SELECT c.oid, c.relkind::pg_catalog.text AS relkind
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
			WHERE n.nspname OPERATOR(pg_catalog.=) $1
			  AND c.relname OPERATOR(pg_catalog.=) $2
		)
		SELECT t.relkind, ARRAY(
			SELECT i.relname
			FROM pg_catalog.pg_constraint c
			JOIN pg_catalog.pg_class i ON i.oid OPERATOR(pg_catalog.=) c.conindid
			WHERE c.conrelid OPERATOR(pg_catalog.=) t.oid
			  AND (c.contype OPERATOR(pg_catalog.=) 'p'
			    OR c.contype OPERATOR(pg_catalog.=) 'u'
			    OR c.contype OPERATOR(pg_catalog.=) 'x')
			ORDER BY i.relname
		), ARRAY(
			SELECT s.relname
			FROM pg_catalog.pg_depend d
			JOIN pg_catalog.pg_class s ON s.oid OPERATOR(pg_catalog.=) d.objid
			WHERE d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND d.refobjid OPERATOR(pg_catalog.=) t.oid
			  AND d.refobjsubid OPERATOR(pg_catalog.>) 0
			  AND d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND (d.deptype OPERATOR(pg_catalog.=) 'a'
			    OR d.deptype OPERATOR(pg_catalog.=) 'i')
			  AND s.relkind OPERATOR(pg_catalog.=) 'S'
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
