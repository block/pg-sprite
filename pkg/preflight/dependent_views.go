package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ColumnDependentViews maps each column of schema.table that a view or
// materialized view reads to those views, each schema-qualified with every
// identifier quoted, sorted. A column no view reads is absent.
//
// PostgreSQL refuses to change the type of a column a view reads, even a
// relabel that rewrites nothing, and refuses to drop one without CASCADE.
// The dependency is the one pg_depend records from the view's rewrite rule
// to the column, so a view that reads the table without naming the column
// does not appear. The read is a planning fact: the server's own refusal at
// execution stays the backstop for a view created after it.
func ColumnDependentViews(ctx context.Context, pool *pgxpool.Pool, schema, table string) (map[string][]string, error) {
	rows, err := pool.Query(ctx, `SELECT a.attname::text, n.nspname::text, v.relname::text
		FROM pg_catalog.pg_depend d
		JOIN pg_catalog.pg_rewrite r ON r.oid = d.objid
		JOIN pg_catalog.pg_class v ON v.oid = r.ev_class
		JOIN pg_catalog.pg_namespace n ON n.oid = v.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE d.classid = 'pg_catalog.pg_rewrite'::pg_catalog.regclass
		  AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
		  AND d.refobjid = pg_catalog.to_regclass(pg_catalog.quote_ident($1) || '.' || pg_catalog.quote_ident($2))
		  AND d.refobjsubid > 0
		  AND v.oid <> d.refobjid
		  AND v.relkind IN ('v', 'm')
		GROUP BY a.attname, n.nspname, v.relname
		ORDER BY a.attname, n.nspname, v.relname`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("read the views that depend on the columns of %s: %w", pgx.Identifier{schema, table}.Sanitize(), err)
	}
	views := map[string][]string{}
	for rows.Next() {
		var column, viewSchema, view string
		if err := rows.Scan(&column, &viewSchema, &view); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the views that depend on the columns of %s: %w", pgx.Identifier{schema, table}.Sanitize(), err)
		}
		views[column] = append(views[column], pgx.Identifier{viewSchema, view}.Sanitize())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the views that depend on the columns of %s: %w", pgx.Identifier{schema, table}.Sanitize(), err)
	}
	return views, nil
}
