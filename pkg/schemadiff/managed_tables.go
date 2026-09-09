package schemadiff

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const listManagedTablesSQL = `
	SELECT c.relname
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n
	  ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
	WHERE n.nspname OPERATOR(pg_catalog.=) $1
	  AND (
	      c.relkind OPERATOR(pg_catalog.=) 'r'
	      OR c.relkind OPERATOR(pg_catalog.=) 'p'
	  )
	  AND NOT c.relispartition
	  AND NOT EXISTS (
	      SELECT 1
	      FROM pg_catalog.pg_depend d
	      WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
	        AND d.objid OPERATOR(pg_catalog.=) c.oid
	        AND d.deptype OPERATOR(pg_catalog.=) 'e'
	  )
	ORDER BY c.relname`

// ListManagedTables returns, sorted by name, the tables of a live PostgreSQL
// schema that a declarative schema directory is expected to account for: the
// set pull enumerates before it exports each one. Ordinary tables and
// partitioned parents are listed; partitions are represented by their
// parent's PARTITION BY and extension members belong to their extension, so
// neither is a table an owner declares. The listing is the candidate set, not
// the set of files pull writes: a listed table whose shape Render refuses (a
// partitioned parent, either side of INHERITS, an unlogged table, a table
// other tables reference) is still undeclared and still the owner's to
// resolve, and pull reports the refusal by table. Catalog objects and
// operators are fully qualified so search_path shadowing cannot turn this
// fail-closed enumeration into a silent empty result.
func ListManagedTables(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	rows, err := pool.Query(ctx, listManagedTablesSQL, schema)
	if err != nil {
		return nil, fmt.Errorf("list managed tables in schema %q: %w", schema, err)
	}
	defer rows.Close()

	tables := make([]string, 0)
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("list managed tables in schema %q: %w", schema, err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list managed tables in schema %q: %w", schema, err)
	}
	return tables, nil
}
