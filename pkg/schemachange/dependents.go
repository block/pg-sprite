package schemachange

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DependentKind says which kind of table dependent a pairing describes.
type DependentKind string

const (
	// DependentIndex is an index, constraint-backed or not.
	DependentIndex DependentKind = "index"
	// DependentStatistics is an extended-statistics object.
	DependentStatistics DependentKind = "statistics"
)

// DependentPair is one source dependent and the shadow dependent that
// cutover renames to its name (D8). The two are paired by definition, never
// by name: LIKE derives every shadow dependent's name from the shadow's.
type DependentPair struct {
	// Kind is the dependent's kind.
	Kind DependentKind `json:"kind"`
	// SourceName is the name the source dependent holds, which the shadow
	// dependent takes after the swap.
	SourceName string `json:"source_name"`
	// ShadowName is the name the shadow dependent holds before the swap.
	ShadowName string `json:"shadow_name"`
}

// DependentPairing is the outcome of pairing one kind of dependent between
// the source and its shadow. A dependent without a partner is not a fault:
// the gated statement may have dropped a source index's column or added an
// index of its own. Cutover renames only the pairs; an unpaired shadow
// dependent keeps its shadow-derived name, and an unpaired source
// dependent ends with the old table.
type DependentPairing struct {
	// Pairs are the dependents cutover renames, in source-name order.
	Pairs []DependentPair `json:"pairs"`
	// UnpairedSource are the source dependents no shadow dependent matches.
	UnpairedSource []string `json:"unpaired_source"`
	// UnpairedShadow are the shadow dependents no source dependent matches.
	UnpairedShadow []string `json:"unpaired_shadow"`
}

// dependent is one catalog dependent: its name and the definition it is
// paired on.
type dependent struct {
	name string
	// key is the definition rendered name-free, so two dependents with the
	// same key would be indistinguishable once renamed.
	key string
}

// pairByDefinition pairs source dependents with shadow dependents whose
// definitions are equal. Two source dependents with the same definition
// are interchangeable, so each takes the next unpaired shadow dependent
// with that definition (D8). The source order decides the pair order.
func pairByDefinition(kind DependentKind, source, shadow []dependent) DependentPairing {
	free := make(map[string][]string)
	for _, d := range shadow {
		free[d.key] = append(free[d.key], d.name)
	}
	pairing := DependentPairing{
		Pairs:          []DependentPair{},
		UnpairedSource: []string{},
		UnpairedShadow: []string{},
	}
	for _, d := range source {
		candidates := free[d.key]
		if len(candidates) == 0 {
			pairing.UnpairedSource = append(pairing.UnpairedSource, d.name)
			continue
		}
		pairing.Pairs = append(pairing.Pairs, DependentPair{Kind: kind, SourceName: d.name, ShadowName: candidates[0]})
		free[d.key] = candidates[1:]
	}
	for _, d := range shadow {
		if slices.Contains(free[d.key], d.name) {
			pairing.UnpairedShadow = append(pairing.UnpairedShadow, d.name)
		}
	}
	return pairing
}

// indexDefinition is the name-free definition of one index: everything
// that decides which rows and values it covers and how, plus the kind of
// constraint it backs, since cutover restores a constraint name together
// with its index. Storage parameters and tablespace are not part of it; two
// indexes that differ only there deliver the same schema.
type indexDefinition struct {
	AccessMethod string `json:"access_method"`
	Unique       bool   `json:"unique"`
	Primary      bool   `json:"primary"`
	// NullsNotDistinct is pg_index.indnullsnotdistinct: whether a unique
	// index treats two NULL keys as duplicates. Servers without the column
	// read it as false, which is the only behaviour they have.
	NullsNotDistinct bool     `json:"nulls_not_distinct"`
	KeyColumns       []string `json:"key_columns"`
	Included         []string `json:"included"`
	// Opclasses and Collations are per key column, schema-qualified; the
	// collation is empty for a column whose type is not collatable.
	Opclasses  []string `json:"opclasses"`
	Collations []string `json:"collations"`
	// DefaultOpclasses and OwnCollations are per key column: whether the
	// operator class is the default for its type, and whether the collation
	// is the key column's own rather than one written in the index. They
	// say where each fact came from, not what it is, so they are not part
	// of the rendered definition; they decide which facts the relaxed
	// pairing across a retyped column may set aside.
	DefaultOpclasses []bool `json:"-"`
	OwnCollations    []bool `json:"-"`
	// Options are pg_index.indoption per key column (DESC and NULLS FIRST
	// flags), which the per-column pg_get_indexdef form does not print.
	Options   []int16 `json:"options"`
	Predicate string  `json:"predicate"`
	// Constraint is pg_get_constraintdef of the constraint the index
	// backs, empty for a plain index; it carries deferrability and the
	// exclusion operators, none of which pg_index records.
	Constraint string `json:"constraint"`
}

// indexEntry is one index as read from the catalog.
type indexEntry struct {
	name       string
	valid      bool
	definition indexDefinition
}

// readIndexes lists the table's indexes with their name-free definitions.
// Every column reference comes back by name (pg_get_indexdef with a column
// position prints the column name or expression), never by attnum, because
// a source with a dropped column numbers its columns differently from the
// shadow LIKE built. The NULLS NOT DISTINCT flag is read through the row's
// JSON rendering so the query parses on servers whose pg_index lacks it.
func readIndexes(ctx context.Context, tx pgx.Tx, oid uint32) ([]indexEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT ic.relname, i.indisvalid, am.amname, i.indisunique, i.indisprimary,
		       COALESCE((to_jsonb(i) ->> 'indnullsnotdistinct')::bool, false),
		       ARRAY(SELECT pg_get_indexdef(i.indexrelid, k, true)
		             FROM generate_series(1, i.indnkeyatts) k),
		       ARRAY(SELECT pg_get_indexdef(i.indexrelid, k, true)
		             FROM generate_series(i.indnkeyatts + 1, i.indnatts) k),
		       ARRAY(SELECT oc.opcnamespace::regnamespace::text || '.' || oc.opcname
		             FROM unnest(i.indclass::oid[]) WITH ORDINALITY u(o, k)
		             JOIN pg_opclass oc ON oc.oid = u.o
		             ORDER BY k),
		       ARRAY(SELECT COALESCE(co.collnamespace::regnamespace::text || '.' || co.collname, '')
		             FROM unnest(i.indcollation::oid[]) WITH ORDINALITY u(o, k)
		             LEFT JOIN pg_collation co ON co.oid = u.o
		             ORDER BY k),
		       ARRAY(SELECT oc.opcdefault
		             FROM unnest(i.indclass::oid[]) WITH ORDINALITY u(o, k)
		             JOIN pg_opclass oc ON oc.oid = u.o
		             ORDER BY k),
		       ARRAY(SELECT u.o = COALESCE(a.attcollation, u.o)
		             FROM unnest(i.indcollation::oid[], i.indkey::int2[]) WITH ORDINALITY u(o, n, k)
		             LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = u.n AND u.n > 0
		             WHERE u.k <= i.indnkeyatts
		             ORDER BY k),
		       ARRAY(SELECT u.o FROM unnest(i.indoption::int2[]) WITH ORDINALITY u(o, k) ORDER BY k),
		       COALESCE(pg_get_expr(i.indpred, i.indrelid), ''),
		       COALESCE(pg_get_constraintdef(con.oid), '')
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_am am ON am.oid = ic.relam
		LEFT JOIN pg_constraint con
		       ON con.conindid = i.indexrelid AND con.conrelid = i.indrelid AND con.contype IN ('p', 'u', 'x')
		WHERE i.indrelid = $1
		ORDER BY ic.relname`, oid)
	if err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	indexes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (indexEntry, error) {
		var e indexEntry
		d := &e.definition
		err := row.Scan(&e.name, &e.valid, &d.AccessMethod, &d.Unique, &d.Primary, &d.NullsNotDistinct,
			&d.KeyColumns, &d.Included, &d.Opclasses, &d.Collations, &d.DefaultOpclasses, &d.OwnCollations,
			&d.Options, &d.Predicate, &d.Constraint)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	return indexes, nil
}

// indexDependents renders the indexes as pairing inputs, each definition
// relaxed across the retyped columns when any are given.
func indexDependents(indexes []indexEntry, retyped map[string]bool) ([]dependent, error) {
	out := make([]dependent, 0, len(indexes))
	for _, e := range indexes {
		key, err := json.Marshal(e.definition.relaxedAcross(retyped))
		if err != nil {
			return nil, fmt.Errorf("render definition of index %s: %w", e.name, err)
		}
		out = append(out, dependent{name: e.name, key: string(key)})
	}
	return out, nil
}

// invalidIndexes names the indexes whose pg_index.indisvalid is false.
func invalidIndexes(indexes []indexEntry) []string {
	var names []string
	for _, e := range indexes {
		if !e.valid {
			names = append(names, e.name)
		}
	}
	return names
}

// readStatistics lists the table's extended-statistics objects as pairing
// inputs: the sorted statistics kinds and the server's name-free column
// list decide the definition.
func readStatistics(ctx context.Context, tx pgx.Tx, oid uint32) ([]dependent, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.stxname,
		       array_to_string(ARRAY(SELECT k FROM unnest(s.stxkind) k ORDER BY k), ','),
		       pg_get_statisticsobjdef_columns(s.oid)
		FROM pg_statistic_ext s
		WHERE s.stxrelid = $1
		ORDER BY s.stxname`, oid)
	if err != nil {
		return nil, fmt.Errorf("read extended statistics: %w", err)
	}
	stats, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dependent, error) {
		var d dependent
		var kinds, columns string
		if err := row.Scan(&d.name, &kinds, &columns); err != nil {
			return dependent{}, err
		}
		d.key = kinds + " ON " + columns
		return d, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read extended statistics: %w", err)
	}
	return stats, nil
}

// sourceNames lists the source-side names of a pairing: the paired and the
// unpaired source dependents, each of which cutover renames (D8).
func (p DependentPairing) sourceNames() []string {
	names := make([]string, 0, len(p.Pairs)+len(p.UnpairedSource))
	for _, pair := range p.Pairs {
		names = append(names, pair.SourceName)
	}
	return append(names, p.UnpairedSource...)
}

// oldNames lists the names cutover gives the source and its dependents
// (D8): the retained table's name and one derived name per dependent.
// Every one must be free before the swap starts.
func oldNames(schema, table string, dependents []string) []string {
	names := []string{OldName(schema, table)}
	for _, d := range dependents {
		names = append(names, OldDependentName(schema, table, d))
	}
	return names
}

// takenNames returns, of the candidate names, those already worn by a
// relation or an extended-statistics object in the schema.
func takenNames(ctx context.Context, tx pgx.Tx, schema string, candidates []string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = ANY ($2)
		UNION
		SELECT s.stxname
		FROM pg_statistic_ext s
		JOIN pg_namespace n ON n.oid = s.stxnamespace
		WHERE n.nspname = $1 AND s.stxname = ANY ($2)
		ORDER BY 1`, schema, candidates)
	if err != nil {
		return nil, fmt.Errorf("check derived names in schema %s: %w", schema, err)
	}
	taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("check derived names in schema %s: %w", schema, err)
	}
	return taken, nil
}

// constraintNamesTaken returns, of the names that paired, constraint-backed
// shadow indexes will take, those a constraint on the shadow that is not
// backed by one of those indexes already holds. ALTER INDEX … RENAME renames
// the backing constraint with the index, and constraint names are unique
// per table, so such a name would make the rename fail under the cutover
// lock. A plain index renames nothing in pg_constraint, so a constraint
// sharing its target name is no collision.
func constraintNamesTaken(ctx context.Context, tx pgx.Tx, shadowOID uint32, indexes DependentPairing, shadow []indexEntry) ([]string, error) {
	backsConstraint := make(map[string]bool, len(shadow))
	for _, e := range shadow {
		backsConstraint[e.name] = e.definition.Constraint != ""
	}
	targets := make([]string, 0, len(indexes.Pairs))
	paired := make([]string, 0, len(indexes.Pairs))
	for _, pair := range indexes.Pairs {
		if !backsConstraint[pair.ShadowName] {
			continue
		}
		targets = append(targets, pair.SourceName)
		paired = append(paired, pair.ShadowName)
	}
	rows, err := tx.Query(ctx, `
		SELECT con.conname
		FROM pg_constraint con
		LEFT JOIN pg_class ic ON ic.oid = con.conindid AND ic.relname = ANY ($3)
		WHERE con.conrelid = $1 AND con.conname = ANY ($2) AND ic.oid IS NULL
		ORDER BY con.conname`, shadowOID, targets, paired)
	if err != nil {
		return nil, fmt.Errorf("check shadow constraint names: %w", err)
	}
	taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("check shadow constraint names: %w", err)
	}
	return taken, nil
}

// joinNames renders names for a refusal detail.
func joinNames(names []string) string {
	return strings.Join(names, ", ")
}
