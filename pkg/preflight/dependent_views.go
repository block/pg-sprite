package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DependentViews names, per column, the views and materialized views that
// read it, each schema-qualified with every identifier quoted, sorted. A
// column no view reads is absent from both maps.
type DependentViews struct {
	// Tree covers the table and every table below it, partitions and
	// inheritance children alike: what a recursive ALTER TABLE changes.
	Tree map[string][]string
	// Own covers the table alone: what an ALTER TABLE ONLY changes.
	Own map[string][]string
}

// ColumnDependentViews reads which views and materialized views read each
// column of schema.table, and of the same-named column of every table
// below it. A column only a table below has is left out: no change to the
// table reaches it.
//
// PostgreSQL refuses to change the type of a column a view reads, even a
// relabel that rewrites nothing, and refuses to drop one without CASCADE.
// A recursive change reaches the column on every table below, so a view on
// a child blocks it as surely as one on the table. The dependency is the
// one pg_depend records from the view's rewrite rule to the column, so a
// view that reads the table without naming the column does not appear. The
// read is a planning fact: the server's own refusal at execution stays the
// backstop for a view created after it.
func ColumnDependentViews(ctx context.Context, pool *pgxpool.Pool, schema, table string) (DependentViews, error) {
	name := pgx.Identifier{schema, table}.Sanitize()
	rows, err := pool.Query(ctx, `WITH RECURSIVE tree(oid, depth) AS (
			SELECT pg_catalog.to_regclass(pg_catalog.quote_ident($1) || '.' || pg_catalog.quote_ident($2))::oid, 0
			UNION
			SELECT i.inhrelid, tree.depth + 1
			FROM tree
			JOIN pg_catalog.pg_inherits i ON i.inhparent = tree.oid
		)
		SELECT a.attname::text, n.nspname::text, v.relname::text, pg_catalog.bool_or(tree.depth = 0)
		FROM tree
		JOIN pg_catalog.pg_depend d ON d.refobjid = tree.oid
		JOIN pg_catalog.pg_rewrite r ON r.oid = d.objid
		JOIN pg_catalog.pg_class v ON v.oid = r.ev_class
		JOIN pg_catalog.pg_namespace n ON n.oid = v.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE d.classid = 'pg_catalog.pg_rewrite'::pg_catalog.regclass
		  AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
		  AND d.refobjsubid > 0
		  AND v.oid <> d.refobjid
		  AND v.relkind IN ('v', 'm')
		  AND EXISTS (SELECT 1 FROM pg_catalog.pg_attribute ra
		              WHERE ra.attrelid = (SELECT oid FROM tree WHERE depth = 0)
		                AND ra.attname = a.attname AND ra.attnum > 0 AND NOT ra.attisdropped)
		GROUP BY a.attname, n.nspname, v.relname
		ORDER BY a.attname, n.nspname, v.relname`, schema, table)
	if err != nil {
		return DependentViews{}, fmt.Errorf("read the views that depend on the columns of %s: %w", name, err)
	}
	views := DependentViews{Tree: map[string][]string{}, Own: map[string][]string{}}
	for rows.Next() {
		var column, viewSchema, view string
		var own bool
		if err := rows.Scan(&column, &viewSchema, &view, &own); err != nil {
			rows.Close()
			return DependentViews{}, fmt.Errorf("read the views that depend on the columns of %s: %w", name, err)
		}
		qualified := pgx.Identifier{viewSchema, view}.Sanitize()
		views.Tree[column] = append(views.Tree[column], qualified)
		if own {
			views.Own[column] = append(views.Own[column], qualified)
		}
	}
	if err := rows.Err(); err != nil {
		return DependentViews{}, fmt.Errorf("read the views that depend on the columns of %s: %w", name, err)
	}
	return views, nil
}
