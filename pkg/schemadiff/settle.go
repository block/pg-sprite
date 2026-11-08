package schemadiff

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// maxSettleRounds bounds how many times a desired model's expressions are
// replayed. Each nesting level of a row comparison needs one replay to
// expand, and the bound keeps an expression that never stops changing from
// holding the scratch transaction open.
const maxSettleRounds = 4

// ErrExpressionUnsettled reports a desired expression whose printed text was
// still changing after maxSettleRounds replays, so no text on its chain can
// be trusted to be the last one a live table might hold.
var ErrExpressionUnsettled = errors.New("the server prints a desired expression differently each time it parses it")

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
// The chain also runs the other way: a desired file pulled from the live
// table holds the live text, and building it prints the next text, one step
// ahead of the live one. recordLivePrecursors replays each live text that is
// not on the chain and records it when it prints onto the chain.
//
// The replayed expressions are check constraints, column defaults,
// generation expressions on servers that can replace one (PostgreSQL 17),
// and the definitions of indexes no constraint owns that index an
// expression or carry a predicate. An index on plain columns prints the
// same way however often it is parsed, and leaving it alone keeps a replica
// identity or a foreign key that rests on it in place. The scratch table
// is empty, so each replay is a catalog change. A model with nothing to
// replay is returned as it is, without a further introspection.
func settleDesired(ctx context.Context, tx pgx.Tx, scratch, target, table string, path []string, m Model) (Model, error) {
	r, err := newReplayer(ctx, tx, scratch, table, path, m)
	if err != nil {
		return Model{}, err
	}
	stmts := r.statements(m)
	if len(stmts) == 0 {
		return m, nil
	}
	settled := cloneReplayed(m)
	current := m
	for round := 0; ; round++ {
		if err := r.run(ctx, tx, stmts); err != nil {
			return Model{}, err
		}
		next, err := introspectOnPath(ctx, tx, scratch, table, path, true)
		if err != nil {
			return Model{}, fmt.Errorf("introspect replayed desired state: %w", err)
		}
		if reflect.DeepEqual(next, current) {
			break
		}
		if round+1 == maxSettleRounds {
			return Model{}, unsettledError(current, next)
		}
		if err := recordReplays(&settled, current, next); err != nil {
			return Model{}, err
		}
		current = next
		stmts = r.statements(current)
	}
	return r.recordLivePrecursors(ctx, target, settled)
}

// unsettledError names a definition whose text changed in the last replay.
func unsettledError(before, after Model) error {
	name, text := "an expression", ""
	for i := range after.Columns {
		if i < len(before.Columns) && after.Columns[i].Default != before.Columns[i].Default {
			name, text = fmt.Sprintf("the default of column %q", after.Columns[i].Name), after.Columns[i].Default
		}
	}
	for i := range after.Constraints {
		if i < len(before.Constraints) && after.Constraints[i].Def != before.Constraints[i].Def {
			name, text = fmt.Sprintf("constraint %q", after.Constraints[i].Name), after.Constraints[i].Def
		}
	}
	for i := range after.Indexes {
		if i < len(before.Indexes) && after.Indexes[i].Def != before.Indexes[i].Def {
			name, text = fmt.Sprintf("index %q", after.Indexes[i].Name), after.Indexes[i].Def
		}
	}
	return fmt.Errorf("%w: %s of table %s still changed after %d replays, to %s; write it without nested row comparisons",
		ErrExpressionUnsettled, name, after.Table, maxSettleRounds, text)
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
// two equal. A replay is the same definition after more or fewer parses,
// never a different one.
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
		l, ok := liveCons[c.Name]
		if !ok {
			continue
		}
		// A live constraint left NOT VALID prints its definition with the
		// suffix, which no replay carries. The replay is matched without
		// it and the desired constraint stays validated, so the diff plans
		// the VALIDATE instead of a drop and add.
		if def := liveDefWithoutNotValid(l); slices.Contains(c.DefReplays, def) {
			desired.Constraints[i].Def = def
		}
	}
	for i, ix := range desired.Indexes {
		if l, ok := liveIdx[ix.Name]; ok && slices.Contains(ix.DefReplays, l.Def) {
			desired.Indexes[i].Def = l.Def
		}
	}
	return desired
}

// replayer runs a model's replayable expressions back through the server on
// the scratch table.
type replayer struct {
	tx                    pgx.Tx
	scratch, table        string
	path                  []string
	canReplaceExpressions bool
	// expressive names the indexes that index an expression or carry a
	// predicate. A replay never changes which indexes exist, so it is read
	// once.
	expressive map[string]struct{}
}

// newReplayer reads what the scratch table's replays depend on. The server
// version is read only when m has a generation expression to replay.
func newReplayer(ctx context.Context, tx pgx.Tx, scratch, table string, path []string, m Model) (replayer, error) {
	r := replayer{tx: tx, scratch: scratch, table: table, path: path}
	if slices.ContainsFunc(m.Columns, func(c Column) bool { return c.Generated && hasReplayableExpression(c) }) {
		can, err := serverCanReplaceGenerationExpression(ctx, tx)
		if err != nil {
			return replayer{}, err
		}
		r.canReplaceExpressions = can
	}
	expressive, err := indexesWithExpressions(ctx, tx, scratch, table)
	if err != nil {
		return replayer{}, err
	}
	r.expressive = expressive
	return r, nil
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

// statements returns the statements that set each replayable expression of
// m on the scratch table to the text m carries for it.
func (r replayer) statements(m Model) []string {
	target := pgx.Identifier{r.scratch, r.table}.Sanitize()
	var stmts []string
	for _, col := range m.Columns {
		if sql, ok := replayColumnExpression(target, col, r.canReplaceExpressions); ok {
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
	for _, ix := range m.Indexes {
		if _, ok := r.expressive[ix.Name]; !ok {
			continue
		}
		stmts = append(stmts, "DROP INDEX "+pgx.Identifier{r.scratch, ix.Name}.Sanitize(), ix.Def)
	}
	return stmts
}

// run executes stmts on tx under the build's search_path, so the
// unqualified names the server printed resolve as they did.
func (r replayer) run(ctx context.Context, tx pgx.Tx, stmts []string) error {
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(r.path...)); err != nil {
		return fmt.Errorf("set replay search_path: %w", err)
	}
	for _, sql := range stmts {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("replay desired expression on scratch schema: %s: %w", sql, err)
		}
	}
	return nil
}

// recordLivePrecursors records on settled each live text that is not on its
// definition's chain but prints onto it when replayed on the scratch table:
// the text a desired file pulled from the live table carries, one replay
// behind what building that file prints. The live text is replayed in a
// savepoint that is always rolled back, so the scratch table keeps the
// settled texts. A live table that does not exist yet has nothing to match.
func (r replayer) recordLivePrecursors(ctx context.Context, target string, settled Model) (Model, error) {
	if target == "" {
		return settled, nil
	}
	live, err := introspectOnPath(ctx, r.tx, target, r.table, searchPathFor(target), false)
	if errors.Is(err, ErrTableNotFound) {
		return settled, nil
	}
	if err != nil {
		return Model{}, fmt.Errorf("introspect live %s.%s to match its expressions: %w", target, r.table, err)
	}
	liveCols := make(map[string]Column, len(live.Columns))
	for _, c := range live.Columns {
		liveCols[c.Name] = c
	}
	for i, c := range settled.Columns {
		l, ok := liveCols[c.Name]
		if !ok || l.Generated != c.Generated || !hasReplayableExpression(c) || onChain(l.Default, c.Default, c.DefaultReplays) {
			continue
		}
		candidate := c
		candidate.Default = l.Default
		after, ok, err := r.replayInSavepoint(ctx, Model{Columns: []Column{candidate}})
		if err != nil {
			return Model{}, err
		}
		if ok && onChain(columnDefault(after, c.Name), c.Default, c.DefaultReplays) {
			settled.Columns[i].DefaultReplays = append(settled.Columns[i].DefaultReplays, l.Default)
		}
	}
	liveCons := constraintsByName(live.Constraints)
	for i, c := range settled.Constraints {
		l, ok := liveCons[c.Name]
		if !ok || !strings.HasPrefix(c.Def, "CHECK ") {
			continue
		}
		def := liveDefWithoutNotValid(l)
		if !strings.HasPrefix(def, "CHECK ") || onChain(def, c.Def, c.DefReplays) {
			continue
		}
		candidate := c
		candidate.Def = def
		after, ok, err := r.replayInSavepoint(ctx, Model{Constraints: []Constraint{candidate}})
		if err != nil {
			return Model{}, err
		}
		if ok && onChain(constraintsByName(after.Constraints)[c.Name].Def, c.Def, c.DefReplays) {
			settled.Constraints[i].DefReplays = append(settled.Constraints[i].DefReplays, def)
		}
	}
	liveIdx := indexesByName(live.Indexes)
	for i, ix := range settled.Indexes {
		l, ok := liveIdx[ix.Name]
		if _, expressive := r.expressive[ix.Name]; !ok || !expressive || onChain(l.Def, ix.Def, ix.DefReplays) {
			continue
		}
		// The desired text's ON clause is proven to name the scratch
		// table. A live text naming its table any other way is not
		// replayed, so the replay can never build an index on a live table.
		if !sameIndexTarget(l.Def, ix.Def) {
			continue
		}
		after, ok, err := r.replayInSavepoint(ctx, Model{Indexes: []Index{{Name: ix.Name, Def: l.Def}}})
		if err != nil {
			return Model{}, err
		}
		if ok && onChain(indexesByName(after.Indexes)[ix.Name].Def, ix.Def, ix.DefReplays) {
			settled.Indexes[i].DefReplays = append(settled.Indexes[i].DefReplays, l.Def)
		}
	}
	return settled, nil
}

// replayInSavepoint replays m's expressions on the scratch table in a
// savepoint, introspects the result, and rolls the savepoint back. It
// reports false when the server refuses the replay with a class 42 error,
// such as a column or function the desired table does not have: the live
// text does not apply to the desired table, which the diff plans as the
// change it is.
func (r replayer) replayInSavepoint(ctx context.Context, m Model) (Model, bool, error) {
	sp, err := r.tx.Begin(ctx)
	if err != nil {
		return Model{}, false, fmt.Errorf("begin live expression replay: %w", err)
	}
	after, replayed, err := r.replayAndIntrospect(ctx, sp, m)
	if rbErr := sp.Rollback(ctx); rbErr != nil && err == nil {
		return Model{}, false, fmt.Errorf("roll back live expression replay: %w", rbErr)
	}
	return after, replayed, err
}

func (r replayer) replayAndIntrospect(ctx context.Context, sp pgx.Tx, m Model) (Model, bool, error) {
	if err := r.run(ctx, sp, r.statements(m)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "42") {
			return Model{}, false, nil
		}
		return Model{}, false, err
	}
	after, err := introspectOnPath(ctx, sp, r.scratch, r.table, r.path, true)
	if err != nil {
		return Model{}, false, fmt.Errorf("introspect replayed live expression: %w", err)
	}
	return after, true, nil
}

// sameIndexTarget reports whether two index definitions agree up to the
// access method: the same kind of index, name, and table.
func sameIndexTarget(a, b string) bool {
	head := func(def string) string {
		before, _, found := strings.Cut(def, " USING ")
		if !found {
			return ""
		}
		return before
	}
	return head(a) != "" && head(a) == head(b)
}

// onChain reports whether text is a definition's first-build text or one of
// its replays.
func onChain(text, first string, replays []string) bool {
	return text == first || slices.Contains(replays, text)
}

// liveDefWithoutNotValid is a live constraint's definition without the
// NOT VALID suffix the server prints for one left unvalidated, which no
// desired text carries: CREATE TABLE always validates.
func liveDefWithoutNotValid(l Constraint) string {
	if l.NotValid {
		return strings.TrimSuffix(l.Def, notValidSuffix)
	}
	return l.Def
}

func columnDefault(m Model, name string) string {
	for _, c := range m.Columns {
		if c.Name == name {
			return c.Default
		}
	}
	return ""
}

// replayColumnExpression is the statement that sets a column's default or
// generation expression to the text the server printed for it.
func replayColumnExpression(target string, col Column, canReplaceExpressions bool) (string, bool) {
	if !hasReplayableExpression(col) {
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

// hasReplayableExpression reports whether a column carries an expression a
// replay can change. A column with no expression, an identity, or a
// sequence-backed default has nothing to settle: a nextval call prints the
// same way however often it is parsed. A virtual generated column's
// expression cannot be replaced in place.
func hasReplayableExpression(col Column) bool {
	return col.Default != "" && col.Identity == IdentityNone && !col.SequenceDefault && !col.Virtual
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
