package schemachange

import (
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// retypedColumns names the columns whose type the gated statement changed:
// the columns the source and the shadow both carry under one name with
// different canonical types. Both models render a type through
// format_type, so two spellings of one type never read as a retype. A
// dropped, added, or renamed column is not retyped; an index on one of
// those finds no partner and stays unpaired.
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

// relaxedAcross returns the definition with the facts PostgreSQL derives
// afresh on a rebuild cleared for every key column the statement retyped:
// an operator class that is the default for the column's type, and a
// collation that is the column's own. The server re-creates an index on a
// retyped column for the new type, re-deriving exactly those two, so the
// shadow's copy of such an index differs from the source's in those facts
// alone while still indexing the same columns the same way (D8). An
// operator class or collation written in the index is kept on the rebuild,
// so it stays in the relaxed definition and keeps two indexes that differ
// only there apart. An expression or a predicate over a retyped column is
// not relaxed: the server may render either differently for the new type,
// so an equal rendering is required.
func (d indexDefinition) relaxedAcross(retyped map[string]bool) indexDefinition {
	relaxed := d
	relaxed.Opclasses = slices.Clone(d.Opclasses)
	relaxed.Collations = slices.Clone(d.Collations)
	for i, column := range d.KeyColumns {
		if !retyped[column] {
			continue
		}
		if i < len(relaxed.Opclasses) && d.defaultOpclass(i) {
			relaxed.Opclasses[i] = ""
		}
		if i < len(relaxed.Collations) && d.ownCollation(i) {
			relaxed.Collations[i] = ""
		}
	}
	return relaxed
}

// defaultOpclass reports whether key column i uses its type's default
// operator class.
func (d indexDefinition) defaultOpclass(i int) bool {
	return i < len(d.DefaultOpclasses) && d.DefaultOpclasses[i]
}

// ownCollation reports whether key column i uses the column's own
// collation.
func (d indexDefinition) ownCollation(i int) bool {
	return i < len(d.OwnCollations) && d.OwnCollations[i]
}

// pairIndexes pairs the two tables' indexes by definition, then pairs what
// is left across the columns the statement retyped: a source index and a
// shadow index neither of which paired exactly pair when their definitions
// are equal once the derived operator class and collation of every retyped
// key column are set aside. Everything else about the definition — access
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

// pairIndexEntries renders both sides' definitions and pairs them. With
// retyped columns given, the definitions are relaxed across them, and a
// relaxed definition that more than one index on either side renders to
// pairs nothing: the exact rule may treat equal definitions as
// interchangeable, but a relaxed definition has facts set aside, so two
// indexes that render alike under it are not shown to be the same index,
// and guessing between them would put each under the other's name.
func pairIndexEntries(source, shadow []indexEntry, retyped map[string]bool) (DependentPairing, error) {
	sourceDependents, err := indexDependents(source, retyped)
	if err != nil {
		return DependentPairing{}, err
	}
	shadowDependents, err := indexDependents(shadow, retyped)
	if err != nil {
		return DependentPairing{}, err
	}
	if len(retyped) == 0 {
		return pairByDefinition(DependentIndex, sourceDependents, shadowDependents), nil
	}
	ambiguous := sharedKeys(sourceDependents, shadowDependents)
	pairing := pairByDefinition(DependentIndex, withoutKeys(sourceDependents, ambiguous), withoutKeys(shadowDependents, ambiguous))
	pairing.UnpairedSource = unpairedNames(sourceDependents, pairing.Pairs, func(p DependentPair) string { return p.SourceName })
	pairing.UnpairedShadow = unpairedNames(shadowDependents, pairing.Pairs, func(p DependentPair) string { return p.ShadowName })
	return pairing, nil
}

// sharedKeys returns every key that more than one dependent on the same
// side renders to.
func sharedKeys(sides ...[]dependent) map[string]bool {
	shared := make(map[string]bool)
	for _, side := range sides {
		seen := make(map[string]bool, len(side))
		for _, d := range side {
			if seen[d.key] {
				shared[d.key] = true
			}
			seen[d.key] = true
		}
	}
	return shared
}

// withoutKeys keeps, in order, the dependents whose key is not listed.
func withoutKeys(dependents []dependent, keys map[string]bool) []dependent {
	kept := make([]dependent, 0, len(dependents))
	for _, d := range dependents {
		if !keys[d.key] {
			kept = append(kept, d)
		}
	}
	return kept
}

// unpairedNames lists, in order, the dependents no pair names on its side.
func unpairedNames(dependents []dependent, pairs []DependentPair, nameOf func(DependentPair) string) []string {
	paired := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		paired[nameOf(p)] = true
	}
	names := []string{}
	for _, d := range dependents {
		if !paired[d.name] {
			names = append(names, d.name)
		}
	}
	return names
}

// indexesNamed keeps, in order, the entries whose names are listed. The
// lists are the residue of the exact pass, so a linear scan per entry is
// as much lookup as they need.
func indexesNamed(indexes []indexEntry, names []string) []indexEntry {
	kept := make([]indexEntry, 0, len(names))
	for _, e := range indexes {
		if slices.Contains(names, e.name) {
			kept = append(kept, e)
		}
	}
	return kept
}
