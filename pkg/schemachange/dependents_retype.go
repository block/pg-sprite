package schemachange

import (
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// retypedColumns names the columns whose type the gated statement changed:
// the columns the source and the shadow both carry under one name with
// different canonical types. A dropped, added, or renamed column is not
// retyped; an index on one of those finds no partner and stays unpaired.
//
// Each name is recorded as pg_get_indexdef prints it — bare when it needs
// no quoting and double-quoted otherwise — so a key column read from the
// catalog can be looked up as is.
func retypedColumns(source, target schemadiff.Model) map[string]bool {
	targetTypes := make(map[string]string, len(target.Columns))
	for _, c := range target.Columns {
		targetTypes[c.Name] = c.Type
	}
	retyped := make(map[string]bool)
	for _, c := range source.Columns {
		targetType, onShadow := targetTypes[c.Name]
		if !onShadow || targetType == c.Type {
			continue
		}
		retyped[c.Name] = true
		retyped[pgx.Identifier{c.Name}.Sanitize()] = true
	}
	return retyped
}

// relaxedAcross returns the definition with the operator class and the
// collation cleared for every key column the statement retyped. PostgreSQL
// re-creates an index on a retyped column for the new type — with the new
// type's default operator class and collation — so the shadow's copy of
// that index differs from the source's in those two facts alone, while
// still indexing the same columns the same way (D8). An expression over a
// retyped column is not relaxed: the server may render the expression
// itself differently for the new type, so an equal rendering is required.
func (d indexDefinition) relaxedAcross(retyped map[string]bool) indexDefinition {
	relaxed := d
	relaxed.Opclasses = slices.Clone(d.Opclasses)
	relaxed.Collations = slices.Clone(d.Collations)
	for i, column := range d.KeyColumns {
		if !retyped[column] {
			continue
		}
		if i < len(relaxed.Opclasses) {
			relaxed.Opclasses[i] = ""
		}
		if i < len(relaxed.Collations) {
			relaxed.Collations[i] = ""
		}
	}
	return relaxed
}

// pairIndexes pairs the two tables' indexes by definition, then pairs what
// is left across the columns the statement retyped: a source index and a
// shadow index neither of which paired exactly pair when their definitions
// are equal once the operator class and collation of every retyped key
// column are set aside. Everything else about the definition — access
// method, uniqueness, columns, DESC / NULLS ordering, predicate, backing
// constraint — must still agree, and an index on a column the statement did
// not retype pairs exactly or not at all. Pairs keep the source order.
func pairIndexes(source, shadow []indexEntry, retyped map[string]bool) (DependentPairing, error) {
	exact, err := pairIndexEntries(source, shadow, nil)
	if err != nil {
		return DependentPairing{}, err
	}
	if len(retyped) == 0 || len(exact.UnpairedSource) == 0 || len(exact.UnpairedShadow) == 0 {
		return exact, nil
	}
	relaxed, err := pairIndexEntries(
		indexesNamed(source, exact.UnpairedSource),
		indexesNamed(shadow, exact.UnpairedShadow),
		retyped,
	)
	if err != nil {
		return DependentPairing{}, err
	}
	partner := make(map[string]string, len(exact.Pairs)+len(relaxed.Pairs))
	for _, pair := range exact.Pairs {
		partner[pair.SourceName] = pair.ShadowName
	}
	for _, pair := range relaxed.Pairs {
		partner[pair.SourceName] = pair.ShadowName
	}
	merged := DependentPairing{
		Pairs:          make([]DependentPair, 0, len(partner)),
		UnpairedSource: relaxed.UnpairedSource,
		UnpairedShadow: relaxed.UnpairedShadow,
	}
	for _, e := range source {
		shadowName, paired := partner[e.name]
		if !paired {
			continue
		}
		merged.Pairs = append(merged.Pairs, DependentPair{Kind: DependentIndex, SourceName: e.name, ShadowName: shadowName})
	}
	return merged, nil
}

// pairIndexEntries renders both sides' definitions — relaxed across the
// retyped columns when any are given — and pairs them.
func pairIndexEntries(source, shadow []indexEntry, retyped map[string]bool) (DependentPairing, error) {
	sourceDependents, err := indexDependents(source, retyped)
	if err != nil {
		return DependentPairing{}, err
	}
	shadowDependents, err := indexDependents(shadow, retyped)
	if err != nil {
		return DependentPairing{}, err
	}
	return pairByDefinition(DependentIndex, sourceDependents, shadowDependents), nil
}

// indexesNamed keeps, in order, the entries whose names are listed.
func indexesNamed(indexes []indexEntry, names []string) []indexEntry {
	kept := make([]indexEntry, 0, len(names))
	for _, e := range indexes {
		if slices.Contains(names, e.name) {
			kept = append(kept, e)
		}
	}
	return kept
}
