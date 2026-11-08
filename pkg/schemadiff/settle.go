package schemadiff

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// maxSettleRounds bounds how many times a desired model's expressions are
// replayed. Each nesting level of a row comparison needs one replay to
// expand, and the bound keeps an expression that never stops changing from
// holding the scratch transaction open.
const maxSettleRounds = 4

// settleDesired replays the expressions of a desired model on its scratch
// table until the text the server prints for them stops changing, and
// records each further text on the model as a replay of the first.
//
// The server parses an expression into a tree and prints the tree back, and
// the printed text does not always parse back into the same tree. A nested
// row comparison such as ((a, b), c) IS DISTINCT FROM ((1, 2), 3) expands
// one row level each time it is parsed. A live table holds the text after
// however many replays its definitions have been through: one when it was
// created from the desired file, more when it was created from an earlier
// export of itself. Any text on that chain is the same definition, so Diff
// takes a live text that matches a replay as matching the desired one
// (adoptReplayedText). The model's own texts stay those of the first build,
// so a table whose definitions need no replay plans exactly as before.
//
// The replayed expressions are check constraints, column defaults,
// generation expressions on servers that can replace one (PostgreSQL 17),
// and the definitions of indexes no constraint owns that index an
// expression or carry a predicate. An index on plain columns prints the
// same way however often it is parsed, and leaving it alone keeps a replica
// identity or a foreign key that rests on it in place. The scratch table
// is empty, so each replay is a catalog change.
func settleDesired(ctx context.Context, tx pgx.Tx, scratch, table string, path []string, m Model) (Model, error) {
	canReplaceExpressions, err := serverCanReplaceGenerationExpression(ctx, tx)
	if err != nil {
		return Model{}, err
	}
	settled := cloneReplayed(m)
	current := m
	for range maxSettleRounds {
		if err := replayExpressions(ctx, tx, scratch, table, path, current, canReplaceExpressions); err != nil {
			return Model{}, err
		}
		next, err := introspectOnPath(ctx, tx, scratch, table, path, true)
		if err != nil {
			return Model{}, fmt.Errorf("introspect replayed desired state: %w", err)
		}
		if reflect.DeepEqual(next, current) {
			return settled, nil
		}
		if err := recordReplays(&settled, current, next); err != nil {
			return Model{}, err
		}
		current = next
	}
	return settled, nil
}

// cloneReplayed copies the slices settleDesired records replays into, so
// recording them never writes through to the model it was given.
func cloneReplayed(m Model) Model {
	m.Columns = append([]Column(nil), m.Columns...)
	m.Constraints = append([]Constraint(nil), m.Constraints...)
	m.Indexes = append([]Index(nil), m.Indexes...)
	return m
}

// recordReplays appends to settled every text that changed between two
// consecutive introspections of the scratch table. A replay changes only
// the text of a definition, never which definitions there are, so anything
// else that changed is an error rather than a replay to record.
func recordReplays(settled *Model, before, after Model) error {
	if len(before.Columns) != len(after.Columns) || len(before.Constraints) != len(after.Constraints) ||
		len(before.Indexes) != len(after.Indexes) {
		return fmt.Errorf("replaying the desired expressions of %s changed its columns, constraints, or indexes", before.Table)
	}
	for i := range after.Columns {
		if before.Columns[i].Name != after.Columns[i].Name {
			return fmt.Errorf("replaying the desired expressions of %s reordered its columns", before.Table)
		}
		if after.Columns[i].Default != before.Columns[i].Default {
			settled.Columns[i].DefaultReplays = append(settled.Columns[i].DefaultReplays, after.Columns[i].Default)
		}
	}
	for i := range after.Constraints {
		if before.Constraints[i].Name != after.Constraints[i].Name {
			return fmt.Errorf("replaying the desired expressions of %s renamed a constraint", before.Table)
		}
		if after.Constraints[i].Def != before.Constraints[i].Def {
			settled.Constraints[i].DefReplays = append(settled.Constraints[i].DefReplays, after.Constraints[i].Def)
		}
	}
	for i := range after.Indexes {
		if before.Indexes[i].Name != after.Indexes[i].Name {
			return fmt.Errorf("replaying the desired expressions of %s renamed an index", before.Table)
		}
		if after.Indexes[i].Def != before.Indexes[i].Def {
			settled.Indexes[i].DefReplays = append(settled.Indexes[i].DefReplays, after.Indexes[i].Def)
		}
	}
	return nil
}

// adoptReplayedText returns desired with each definition whose live text is
// one of its replays carrying the live text instead, so the diff finds the
// two equal. A replay is the same definition after more parses, never a
// different one.
func adoptReplayedText(live, desired Model) Model {
	liveCols := make(map[string]Column, len(live.Columns))
	for _, c := range live.Columns {
		liveCols[c.Name] = c
	}
	liveCons := constraintsByName(live.Constraints)
	liveIdx := indexesByName(live.Indexes)
	desired = cloneReplayed(desired)
	for i, c := range desired.Columns {
		if l, ok := liveCols[c.Name]; ok && slices.Contains(c.DefaultReplays, l.Default) {
			desired.Columns[i].Default = l.Default
		}
	}
	for i, c := range desired.Constraints {
		if l, ok := liveCons[c.Name]; ok && slices.Contains(c.DefReplays, l.Def) {
			desired.Constraints[i].Def = l.Def
		}
	}
	for i, ix := range desired.Indexes {
		if l, ok := liveIdx[ix.Name]; ok && slices.Contains(ix.DefReplays, l.Def) {
			desired.Indexes[i].Def = l.Def
		}
	}
	return desired
}

// serverCanReplaceGenerationExpression reports whether the server has
// ALTER COLUMN ... SET EXPRESSION, which PostgreSQL 17 added. An older
// server's generation expressions are left as the first build printed them.
func serverCanReplaceGenerationExpression(ctx context.Context, tx pgx.Tx) (bool, error) {
	var num int
	if err := tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4").Scan(&num); err != nil {
		return false, fmt.Errorf("read server version: %w", err)
	}
	return num >= 170000, nil
}

// replayExpressions runs each replayable expression of m back through the
// server on the scratch table, under the build's search_path so the
// unqualified names the server printed resolve as they did.
func replayExpressions(ctx context.Context, tx pgx.Tx, scratch, table string, path []string, m Model, canReplaceExpressions bool) error {
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(path...)); err != nil {
		return fmt.Errorf("set replay search_path: %w", err)
	}
	target := pgx.Identifier{scratch, table}.Sanitize()
	var stmts []string
	for _, col := range m.Columns {
		if sql, ok := replayColumnExpression(target, col, canReplaceExpressions); ok {
			stmts = append(stmts, sql)
		}
	}
	for _, con := range m.Constraints {
		if !strings.HasPrefix(con.Def, "CHECK ") {
			continue
		}
		name := pgx.Identifier{con.Name}.Sanitize()
		stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s, ADD CONSTRAINT %s %s", target, name, name, con.Def))
	}
	expressive, err := indexesWithExpressions(ctx, tx, scratch, table)
	if err != nil {
		return err
	}
	for _, ix := range m.Indexes {
		if _, ok := expressive[ix.Name]; !ok {
			continue
		}
		stmts = append(stmts, "DROP INDEX "+pgx.Identifier{scratch, ix.Name}.Sanitize(), ix.Def)
	}
	for _, sql := range stmts {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("replay desired expression on scratch schema: %s: %w", sql, err)
		}
	}
	return nil
}

// replayColumnExpression is the statement that sets a column's default or
// generation expression to the text the server printed for it. A column
// with no expression, an identity, or a sequence-backed default has nothing
// to settle: a nextval call prints the same way however often it is parsed.
func replayColumnExpression(target string, col Column, canReplaceExpressions bool) (string, bool) {
	if col.Default == "" || col.Identity != IdentityNone || col.SequenceDefault || col.Virtual {
		return "", false
	}
	name := pgx.Identifier{col.Name}.Sanitize()
	if col.Generated {
		if !canReplaceExpressions {
			return "", false
		}
		return fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET EXPRESSION AS (%s)", target, name, col.Default), true
	}
	return fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s", target, name, col.Default), true
}

// indexesWithExpressions names the scratch table's indexes that index an
// expression or carry a predicate: the only index text a replay can change.
func indexesWithExpressions(ctx context.Context, tx pgx.Tx, scratch, table string) (map[string]struct{}, error) {
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	rows, err := tx.Query(ctx, `
		SELECT c.relname
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) t.relnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND t.relname OPERATOR(pg_catalog.=) $2
		  AND (i.indexprs IS NOT NULL OR i.indpred IS NOT NULL)`, scratch, table)
	if err != nil {
		return nil, fmt.Errorf("query scratch indexes with expressions: %w", err)
	}
	defer rows.Close()
	names := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan scratch index: %w", err)
		}
		names[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read scratch indexes with expressions: %w", err)
	}
	return names, nil
}
