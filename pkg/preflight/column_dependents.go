package preflight

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ColumnDependents names, per column, the objects whose definitions read
// it and that PostgreSQL will not let a type change or a drop of the
// column outlive: views and row-level security policies.
type ColumnDependents struct {
	// Views are the views and materialized views that read the column,
	// each rendered "schema"."view".
	Views DependentObjects
	// Policies are the row-level security policies whose USING or WITH
	// CHECK expression reads the column, each rendered
	// "policy" ON "schema"."table", the form DROP POLICY takes. A policy
	// counts wherever it is defined: one on another table that reads the
	// column through a subquery blocks the change too.
	Policies DependentObjects
}

// DependentObjects names, per column, the dependent objects of one kind
// each reach of a change meets, every identifier quoted, sorted. A column
// no such object reads is absent from every map.
type DependentObjects struct {
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

// ReadColumnDependents reads which views, materialized views, and
// policies read each column of schema.table, and the same-named column of
// every table below it. A column only a table below has is left out: no
// change to the table reaches it.
//
// PostgreSQL refuses to change the type of a column a view or a policy
// reads, even a relabel that rewrites nothing, and refuses to drop one
// without CASCADE. A recursive type change reaches the column on every
// table below, so a dependent on any of them blocks it. A recursive drop
// reaches less: a child loses its copy only when it does not declare the
// column itself and every parent it inherits the column from loses its
// copy in the same drop. A child that declared the column, merged it on
// INHERIT, or also inherits it from a parent outside the drop keeps its
// copy and its dependents, while one that inherits it along two paths
// from the dropped table loses it. Partitions never declare a column of
// their own, so a drop reaches every partition. The dependency is the one
// pg_depend records from the view's rewrite rule, or from the policy, to
// the column, so a view that reads the table without naming the column
// does not appear. The read is a planning fact: the server's own refusal
// at execution stays the backstop for a dependent created after it.
func ReadColumnDependents(ctx context.Context, pool *pgxpool.Pool, schema, table string) (ColumnDependents, error) {
	name := pgx.Identifier{schema, table}.Sanitize()
	tables, root, err := readDependentTree(ctx, pool, schema, table)
	if err != nil {
		return ColumnDependents{}, fmt.Errorf("read the views and policies that depend on the columns of %s: %w", name, err)
	}
	deps := ColumnDependents{Views: newDependentObjects(), Policies: newDependentObjects()}
	rootTable, ok := tables[root]
	if !ok {
		return deps, nil
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
		var treeViews, dropViews, treePolicies, dropPolicies []dependentName
		for oid, t := range tables {
			c, ok := t.columns[column]
			if !ok {
				continue
			}
			treeViews = append(treeViews, c.views...)
			treePolicies = append(treePolicies, c.policies...)
			if dropped[oid] {
				dropViews = append(dropViews, c.views...)
				dropPolicies = append(dropPolicies, c.policies...)
			}
		}
		own := rootTable.columns[column]
		setDependents(deps.Views.Tree, column, treeViews)
		setDependents(deps.Views.Drop, column, dropViews)
		setDependents(deps.Views.Own, column, own.views)
		setDependents(deps.Policies.Tree, column, treePolicies)
		setDependents(deps.Policies.Drop, column, dropPolicies)
		setDependents(deps.Policies.Own, column, own.policies)
	}
	return deps, nil
}

func newDependentObjects() DependentObjects {
	return DependentObjects{Tree: map[string][]string{}, Drop: map[string][]string{}, Own: map[string][]string{}}
}

// dependentTable is one table at or below the table ReadColumnDependents
// reads.
type dependentTable struct {
	// parents are the tables this one inherits from, inside the tree or
	// not.
	parents []uint32
	columns map[string]*dependentColumn
}

// dependentColumn is a table's copy of a column and the views and
// policies that read it.
type dependentColumn struct {
	local    bool
	inhcount int32
	views    []dependentName
	policies []dependentName
}

// dependentName names a view, or a policy and the table it is defined on.
// policy is empty for a view.
type dependentName struct{ schema, relation, policy string }

// The kinds of dependent readDependentTree reports.
const (
	dependentView   = "view"
	dependentPolicy = "policy"
)

// readDependentTree reads schema.table and every table below it, each
// with its parents, its live columns' inheritance counts, and the views
// and policies that read each column, in one statement so the facts come
// from one snapshot. root is the table's oid; a table that does not exist
// yields no tables.
func readDependentTree(ctx context.Context, pool *pgxpool.Pool, schema, table string) (map[uint32]*dependentTable, uint32, error) {
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
			deps.kind, deps.nspname::pg_catalog.text, deps.relname::pg_catalog.text,
			deps.polname::pg_catalog.text
		FROM tree
		JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) tree.oid
			AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
		LEFT JOIN LATERAL (
			SELECT 'view'::pg_catalog.text AS kind, n.nspname, v.relname,
				NULL::pg_catalog.name AS polname
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
			UNION
			SELECT 'policy'::pg_catalog.text, n.nspname, c.relname, p.polname
			FROM pg_catalog.pg_depend d
			JOIN pg_catalog.pg_policy p ON p.oid OPERATOR(pg_catalog.=) d.objid
			JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) p.polrelid
			JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
			WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_policy'::pg_catalog.regclass
			  AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND d.refobjid OPERATOR(pg_catalog.=) tree.oid
			  AND d.refobjsubid OPERATOR(pg_catalog.=) a.attnum
		) deps ON true`, schema, table)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	tables := map[uint32]*dependentTable{}
	var root uint32
	for rows.Next() {
		var oid uint32
		var isRoot bool
		var parents []uint32
		var column string
		var c dependentColumn
		var kind, depSchema, relation, policy *string
		if err := rows.Scan(&oid, &isRoot, &parents, &column, &c.local, &c.inhcount, &kind, &depSchema, &relation, &policy); err != nil {
			return nil, 0, err
		}
		if isRoot {
			root = oid
		}
		t, ok := tables[oid]
		if !ok {
			t = &dependentTable{parents: parents, columns: map[string]*dependentColumn{}}
			tables[oid] = t
		}
		existing, ok := t.columns[column]
		if !ok {
			existing = &c
			t.columns[column] = existing
		}
		if kind == nil {
			continue
		}
		switch *kind {
		case dependentView:
			existing.views = append(existing.views, dependentName{schema: *depSchema, relation: *relation})
		case dependentPolicy:
			existing.policies = append(existing.policies, dependentName{schema: *depSchema, relation: *relation, policy: *policy})
		default:
			return nil, 0, fmt.Errorf("column %s of table %d has a dependent of unknown kind %q", column, oid, *kind)
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
func droppedCopies(root uint32, column string, tables map[uint32]*dependentTable, children map[uint32][]uint32) map[uint32]bool {
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

// setDependents records dependents under column, deduplicated and sorted
// by schema, then relation, then policy name, bytewise, each rendered
// with every identifier quoted: a view as "schema"."view", a policy as
// "policy" ON "schema"."table". No dependents leaves column absent.
func setDependents(into map[string][]string, column string, deps []dependentName) {
	if len(deps) == 0 {
		return
	}
	sorted := slices.SortedFunc(slices.Values(deps), func(a, b dependentName) int {
		return cmp.Or(cmp.Compare(a.schema, b.schema), cmp.Compare(a.relation, b.relation), cmp.Compare(a.policy, b.policy))
	})
	sorted = slices.Compact(sorted)
	out := make([]string, len(sorted))
	for i, d := range sorted {
		out[i] = d.render()
	}
	into[column] = out
}

// render quotes every identifier of the name: "schema"."view" for a view,
// "policy" ON "schema"."table" for a policy.
func (d dependentName) render() string {
	relation := pgx.Identifier{d.schema, d.relation}.Sanitize()
	if d.policy == "" {
		return relation
	}
	return pgx.Identifier{d.policy}.Sanitize() + " ON " + relation
}
