package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DependentViews names, per column, the views and materialized views that
// read it, each schema-qualified with every identifier quoted, sorted. A
// column no view reads is absent from every map.
type DependentViews struct {
	// Tree covers the table and every table below it, partitions and
	// inheritance children alike: what a recursive ALTER COLUMN TYPE
	// changes.
	Tree map[string][]string
	// Drop covers the table and the tables below it whose copy of the
	// column a recursive DROP COLUMN drops: what that drop changes.
	Drop map[string][]string
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
// A recursive type change reaches the column on every table below, so a
// view on any of them blocks it. A recursive drop reaches less: it drops a
// child's copy only when the child inherits the column from this one
// parent alone and does not also declare it, and descends from there, so a
// child that declared the column, merged it on INHERIT, or inherits it
// from a second parent keeps its copy and its views. Partitions never
// declare a column of their own, so a drop reaches every partition. The
// dependency is the one pg_depend records from the view's rewrite rule to
// the column, so a view that reads the table without naming the column
// does not appear. The read is a planning fact: the server's own refusal
// at execution stays the backstop for a view created after it.
func ColumnDependentViews(ctx context.Context, pool *pgxpool.Pool, schema, table string) (DependentViews, error) {
	name := pgx.Identifier{schema, table}.Sanitize()
	// INV: CO-9 — every relation, function, and operator is
	// pg_catalog-qualified.
	rows, err := pool.Query(ctx, `WITH RECURSIVE tree(oid, depth) AS (
			SELECT pg_catalog.to_regclass(pg_catalog.quote_ident($1) OPERATOR(pg_catalog.||) '.'
				OPERATOR(pg_catalog.||) pg_catalog.quote_ident($2))::pg_catalog.oid, 0
			UNION
			SELECT i.inhrelid, tree.depth OPERATOR(pg_catalog.+) 1
			FROM tree
			JOIN pg_catalog.pg_inherits i ON i.inhparent OPERATOR(pg_catalog.=) tree.oid
		),
		dropped(oid, attname) AS (
			SELECT ra.attrelid, ra.attname
			FROM tree
			JOIN pg_catalog.pg_attribute ra ON ra.attrelid OPERATOR(pg_catalog.=) tree.oid
			WHERE tree.depth OPERATOR(pg_catalog.=) 0
			  AND ra.attnum OPERATOR(pg_catalog.>) 0 AND NOT ra.attisdropped
			UNION
			SELECT ca.attrelid, ca.attname
			FROM dropped
			JOIN pg_catalog.pg_inherits i ON i.inhparent OPERATOR(pg_catalog.=) dropped.oid
			JOIN pg_catalog.pg_attribute ca ON ca.attrelid OPERATOR(pg_catalog.=) i.inhrelid
				AND ca.attname OPERATOR(pg_catalog.=) dropped.attname
			WHERE NOT ca.attisdropped AND NOT ca.attislocal
			  AND ca.attinhcount OPERATOR(pg_catalog.=) 1
		)
		SELECT a.attname::pg_catalog.text, n.nspname::pg_catalog.text, v.relname::pg_catalog.text,
			pg_catalog.bool_or(tree.depth OPERATOR(pg_catalog.=) 0),
			pg_catalog.bool_or(EXISTS (SELECT 1 FROM dropped
				WHERE dropped.oid OPERATOR(pg_catalog.=) tree.oid
				  AND dropped.attname OPERATOR(pg_catalog.=) a.attname))
		FROM tree
		JOIN pg_catalog.pg_depend d ON d.refobjid OPERATOR(pg_catalog.=) tree.oid
		JOIN pg_catalog.pg_rewrite r ON r.oid OPERATOR(pg_catalog.=) d.objid
		JOIN pg_catalog.pg_class v ON v.oid OPERATOR(pg_catalog.=) r.ev_class
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) v.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) d.refobjid
			AND a.attnum OPERATOR(pg_catalog.=) d.refobjsubid
		WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass
		  AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		  AND d.refobjsubid OPERATOR(pg_catalog.>) 0
		  AND v.oid OPERATOR(pg_catalog.<>) d.refobjid
		  AND v.relkind OPERATOR(pg_catalog.=) ANY ('{v,m}'::pg_catalog."char"[])
		  AND EXISTS (SELECT 1 FROM dropped
		              WHERE dropped.oid OPERATOR(pg_catalog.=) (SELECT oid FROM tree WHERE depth OPERATOR(pg_catalog.=) 0)
		                AND dropped.attname OPERATOR(pg_catalog.=) a.attname)
		GROUP BY a.attname, n.nspname, v.relname
		ORDER BY a.attname, n.nspname, v.relname`, schema, table)
	if err != nil {
		return DependentViews{}, fmt.Errorf("read the views that depend on the columns of %s: %w", name, err)
	}
	views := DependentViews{Tree: map[string][]string{}, Drop: map[string][]string{}, Own: map[string][]string{}}
	for rows.Next() {
		var column, viewSchema, view string
		var own, drop bool
		if err := rows.Scan(&column, &viewSchema, &view, &own, &drop); err != nil {
			rows.Close()
			return DependentViews{}, fmt.Errorf("read the views that depend on the columns of %s: %w", name, err)
		}
		qualified := pgx.Identifier{viewSchema, view}.Sanitize()
		views.Tree[column] = append(views.Tree[column], qualified)
		if drop {
			views.Drop[column] = append(views.Drop[column], qualified)
		}
		if own {
			views.Own[column] = append(views.Own[column], qualified)
		}
	}
	if err := rows.Err(); err != nil {
		return DependentViews{}, fmt.Errorf("read the views that depend on the columns of %s: %w", name, err)
	}
	return views, nil
}
