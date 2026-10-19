package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaConstraintNames reads the name of every constraint in schema, each
// table's and each domain's: the names PostgreSQL passes over when it names
// an unnamed constraint added to a table there, since it looks for a name
// no constraint in the schema holds.
func SchemaConstraintNames(ctx context.Context, pool *pgxpool.Pool, schema string) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT c.conname::text
		FROM pg_catalog.pg_constraint c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.connamespace
		WHERE n.nspname = $1`, schema)
	if err != nil {
		return nil, fmt.Errorf("read the constraint names in schema %s: %w", pgx.Identifier{schema}.Sanitize(), err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the constraint names in schema %s: %w", pgx.Identifier{schema}.Sanitize(), err)
	}
	taken := make(map[string]bool, len(names))
	for _, name := range names {
		taken[name] = true
	}
	return taken, nil
}
