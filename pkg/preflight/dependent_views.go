package preflight

import (
	"cmp"
	"context"
	"fmt"
	"slices"

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
// view on any of them blocks it. A recursive drop reaches less: a child
// loses its copy only when it does not declare the column itself and every
// parent it inherits the column from loses its copy in the same drop. A
// child that declared the column, merged it on INHERIT, or also inherits it
// from a parent outside the drop keeps its copy and its views, while one
// that inherits it along two paths from the dropped table loses it.
// Partitions never declare a column of their own, so a drop reaches every
// partition. The dependency is the one pg_depend records from the view's
// rewrite rule to the column, so a view that reads the table without
// naming the column does not appear. The read is a planning fact: the
// server's own refusal at execution stays the backstop for a view created
// after it.
func ColumnDependentViews(ctx context.Context, pool *pgxpool.Pool, schema, table string) (DependentViews, error) {
	name := pgx.Identifier{schema, table}.Sanitize()
	tables, root, err := readViewTree(ctx, pool, schema, table)
	if err != nil {
		return DependentViews{}, fmt.Errorf("read the views that depend on the columns of %s: %w", name, err)
	}
	views := DependentViews{Tree: map[string][]string{}, Drop: map[string][]string{}, Own: map[string][]string{}}
	rootTable, ok := tables[root]
	if !ok {
		return views, nil
	}
	children := map[uint32][]uint32{}
	for oid, t := range tables {
		for _, parent := range t.parents {
			if _, inTree := tables[parent]; inTree {
				children[parent] = append(children[parent], oid)
			}
		}
	}
	for column := range rootTable.columns {
		dropped := droppedCopies(root, column, tables, children)
		var tree, drop []viewName
		for oid, t := range tables {
			c, ok := t.columns[column]
			if !ok {
				continue
			}
			tree = append(tree, c.views...)
			if dropped[oid] {
				drop = append(drop, c.views...)
			}
		}
		setViews(views.Tree, column, tree)
		setViews(views.Drop, column, drop)
		setViews(views.Own, column, rootTable.columns[column].views)
	}
	return views, nil
}

// viewTable is one table at or below the table ColumnDependentViews reads.
type viewTable struct {
	// parents are the tables this one inherits from, inside the tree or
	// not.
	parents []uint32
	columns map[string]*viewColumn
}

// viewColumn is a table's copy of a column and the views that read it.
type viewColumn struct {
	local    bool
	inhcount int32
	views    []viewName
}

type viewName struct{ schema, name string }

// readViewTree reads schema.table and every table below it, each with its
// parents, its live columns' inheritance counts, and the views that read
// each column, in one statement so the facts come from one snapshot. root
// is the table's oid; a table that does not exist yields no tables.
func readViewTree(ctx context.Context, pool *pgxpool.Pool, schema, table string) (map[uint32]*viewTable, uint32, error) {
	// INV: CO-9 — every relation, function, and operator is
	// pg_catalog-qualified.
	rows, err := pool.Query(ctx, `WITH RECURSIVE root(oid) AS (
			SELECT pg_catalog.to_regclass(pg_catalog.quote_ident($1) OPERATOR(pg_catalog.||) '.'
				OPERATOR(pg_catalog.||) pg_catalog.quote_ident($2))::pg_catalog.oid
		),
		tree(oid) AS (
			SELECT oid FROM root
			UNION
			SELECT i.inhrelid
			FROM tree
			JOIN pg_catalog.pg_inherits i ON i.inhparent OPERATOR(pg_catalog.=) tree.oid
		)
		SELECT tree.oid, tree.oid OPERATOR(pg_catalog.=) (SELECT oid FROM root),
			ARRAY(SELECT i.inhparent FROM pg_catalog.pg_inherits i
				WHERE i.inhrelid OPERATOR(pg_catalog.=) tree.oid),
			a.attname::pg_catalog.text, a.attislocal, a.attinhcount::pg_catalog.int4,
			views.nspname::pg_catalog.text, views.relname::pg_catalog.text
		FROM tree
		JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) tree.oid
			AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
		LEFT JOIN LATERAL (
			SELECT DISTINCT n.nspname, v.relname
			FROM pg_catalog.pg_depend d
			JOIN pg_catalog.pg_rewrite r ON r.oid OPERATOR(pg_catalog.=) d.objid
			JOIN pg_catalog.pg_class v ON v.oid OPERATOR(pg_catalog.=) r.ev_class
			JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) v.relnamespace
			WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass
			  AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND d.refobjid OPERATOR(pg_catalog.=) tree.oid
			  AND d.refobjsubid OPERATOR(pg_catalog.=) a.attnum
			  AND v.oid OPERATOR(pg_catalog.<>) tree.oid
			  AND v.relkind OPERATOR(pg_catalog.=) ANY ('{v,m}'::pg_catalog."char"[])
		) views ON true`, schema, table)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	tables := map[uint32]*viewTable{}
	var root uint32
	for rows.Next() {
		var oid uint32
		var isRoot bool
		var parents []uint32
		var column string
		var c viewColumn
		var viewSchema, view *string
		if err := rows.Scan(&oid, &isRoot, &parents, &column, &c.local, &c.inhcount, &viewSchema, &view); err != nil {
			return nil, 0, err
		}
		if isRoot {
			root = oid
		}
		t, ok := tables[oid]
		if !ok {
			t = &viewTable{parents: parents, columns: map[string]*viewColumn{}}
			tables[oid] = t
		}
		existing, ok := t.columns[column]
		if !ok {
			existing = &c
			t.columns[column] = existing
		}
		if view != nil {
			existing.views = append(existing.views, viewName{schema: *viewSchema, name: *view})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return tables, root, nil
}

// droppedCopies returns the tables whose copy of column a recursive DROP
// COLUMN on root drops, root included. It replays the server's walk: each
// dropped table visits its children, and a child's copy goes when the
// child does not declare the column and this visit is the last of the
// parents it inherits the column from; otherwise the visit only lowers the
// child's inheritance count, and the copy survives.
func droppedCopies(root uint32, column string, tables map[uint32]*viewTable, children map[uint32][]uint32) map[uint32]bool {
	dropped := map[uint32]bool{root: true}
	remaining := map[uint32]int32{}
	var visit func(parent uint32)
	visit = func(parent uint32) {
		for _, child := range children[parent] {
			c, ok := tables[child].columns[column]
			if !ok || dropped[child] {
				continue
			}
			if _, seen := remaining[child]; !seen {
				remaining[child] = c.inhcount
			}
			if remaining[child] == 1 && !c.local {
				dropped[child] = true
				visit(child)
				continue
			}
			remaining[child]--
		}
	}
	visit(root)
	return dropped
}

// setViews records views under column, deduplicated and sorted by schema,
// then name, bytewise, each rendered with every identifier quoted. No
// views leaves column absent.
func setViews(into map[string][]string, column string, views []viewName) {
	if len(views) == 0 {
		return
	}
	sorted := slices.SortedFunc(slices.Values(views), func(a, b viewName) int {
		return cmp.Or(cmp.Compare(a.schema, b.schema), cmp.Compare(a.name, b.name))
	})
	sorted = slices.Compact(sorted)
	out := make([]string, len(sorted))
	for i, v := range sorted {
		out[i] = pgx.Identifier{v.schema, v.name}.Sanitize()
	}
	into[column] = out
}
