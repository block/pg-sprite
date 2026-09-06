package preflight

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OwnedRelationNames contains the relation names PostgreSQL assigned to a
// table's constraint indexes and column-owned sequences.
type OwnedRelationNames struct {
	ConstraintIndexes []string
	Sequences         []string
}

// LookupOwnedRelationNames reads the server-chosen relation names owned by an
// ordinary or partitioned table so a caller can verify them after a create
// step. Schema must be explicit because the caller must inspect the exact
// schema covered by its earlier absence proof rather than resolve a possibly
// different search_path target. The read excludes standalone indexes, whose
// own create steps report duplicate-name SQLSTATEs, and the table itself,
// whose name is covered by the caller's absence proof.
func LookupOwnedRelationNames(ctx context.Context, pool *pgxpool.Pool, schema, table string) (OwnedRelationNames, error) {
	if schema == "" {
		return OwnedRelationNames{}, fmt.Errorf("look up owned relation names for %s: empty schema", table)
	}
	const q = `
		WITH target AS (
			SELECT c.oid
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1
			  AND c.relname = $2
			  AND c.relkind IN ('r', 'p')
		)
		SELECT ARRAY(
			SELECT i.relname
			FROM target t
			JOIN pg_constraint c ON c.conrelid = t.oid AND c.conindid <> 0
			JOIN pg_class i ON i.oid = c.conindid
			ORDER BY i.relname
		), ARRAY(
			SELECT s.relname
			FROM target t
			JOIN pg_depend d ON d.refobjid = t.oid
			JOIN pg_class s ON s.oid = d.objid
			WHERE d.refclassid = 'pg_class'::regclass
			  AND d.classid = 'pg_class'::regclass
			  AND d.refobjsubid > 0
			  AND d.deptype IN ('a', 'i')
			  AND s.relkind = 'S'
			ORDER BY s.relname
		)
		FROM target`
	var names OwnedRelationNames
	err := pool.QueryRow(ctx, q, schema, table).Scan(&names.ConstraintIndexes, &names.Sequences)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnedRelationNames{}, fmt.Errorf("%w: %s", ErrTableNotFound, qualifiedName(schema, table))
	}
	if err != nil {
		return OwnedRelationNames{}, fmt.Errorf("look up owned relation names for %s: %w", qualifiedName(schema, table), err)
	}
	return names, nil
}
