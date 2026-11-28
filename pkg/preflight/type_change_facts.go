package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionTimeZone returns the TimeZone setting a session on pool runs
// with: the zone ALTER COLUMN TYPE reads each value in when it converts a
// timestamp column to timestamptz, or a timestamptz column to timestamp.
func SessionTimeZone(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var zone string
	if err := pool.QueryRow(ctx, "SELECT pg_catalog.current_setting('TimeZone')").Scan(&zone); err != nil {
		return "", fmt.Errorf("read the session time zone: %w", err)
	}
	return zone, nil
}

// IndexedColumns names the columns of schema.table that an index reads, as
// a key column or in an index expression or predicate, on the table or on
// any of its partitions or inheritance children, since a type change on
// the table changes the column there too. PostgreSQL records each of those
// reads as a dependency of the index on the column.
func IndexedColumns(ctx context.Context, pool *pgxpool.Pool, schema, table string) (map[string]bool, error) {
	// INV: CO-9 — every function, operator, and relation is
	// pg_catalog-qualified, and the table is found by name, not through
	// the session search_path.
	const q = `
		WITH RECURSIVE tree(relid) AS (
			SELECT c.oid
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
			WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
			UNION
			SELECT i.inhrelid
			FROM pg_catalog.pg_inherits i
			JOIN tree t ON i.inhparent OPERATOR(pg_catalog.=) t.relid
		)
		SELECT DISTINCT a.attname
		FROM tree t
		JOIN pg_catalog.pg_depend d
		  ON d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		 AND d.refobjid OPERATOR(pg_catalog.=) t.relid
		 AND d.refobjsubid OPERATOR(pg_catalog.>) 0
		 AND d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		JOIN pg_catalog.pg_index x ON x.indexrelid OPERATOR(pg_catalog.=) d.objid
		JOIN pg_catalog.pg_attribute a
		  ON a.attrelid OPERATOR(pg_catalog.=) d.refobjid
		 AND a.attnum OPERATOR(pg_catalog.=) d.refobjsubid`
	rows, err := pool.Query(ctx, q, schema, table)
	if err != nil {
		return nil, fmt.Errorf("read the indexed columns of %s: %w", qualifiedName(schema, table), err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the indexed columns of %s: %w", qualifiedName(schema, table), err)
	}
	indexed := make(map[string]bool, len(names))
	for _, name := range names {
		indexed[name] = true
	}
	return indexed, nil
}
