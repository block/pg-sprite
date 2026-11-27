package schemadiff

import (
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// renamedIndexes matches each live index the desired table no longer names
// to a desired index the live table does not name, when the two differ by
// name alone. Such a pair is a rename, and the plan carries it as
// ALTER INDEX ... RENAME TO, a catalog change that keeps the index serving
// throughout. A drop and a rebuild would instead leave the table without the
// index while the new one builds, which for a unique index lets duplicates
// in. The result maps the live name to the desired index.
//
// An unfinished build, an index a new key constraint takes over, an earlier
// run's build of a replacement, and a desired index already matched are
// left out, and each index joins at most one pair. Indexes are visited in
// name order, so two live indexes with the same definition always pair the
// same way.
func renamedIndexes(partitioned bool, live, desired []Index, liveIdx, desiredIdx map[string]Index,
	adopted, resumedBuilds map[string]struct{}) (map[string]Index, error) {
	renamed := make(map[string]Index)
	taken := make(map[string]bool)
	for _, ix := range sortedIndexes(live) {
		if _, kept := desiredIdx[ix.Name]; kept || isUnfinishedBuild(partitioned, ix) || ix.Invalid {
			continue
		}
		if _, ok := adopted[ix.Name]; ok {
			continue
		}
		if _, ok := resumedBuilds[ix.Name]; ok {
			continue
		}
		for _, want := range sortedIndexes(desired) {
			if _, exists := liveIdx[want.Name]; exists || taken[want.Name] {
				continue
			}
			same, err := sameIndexUnderName(ix, want)
			if err != nil {
				return nil, err
			}
			if same {
				renamed[ix.Name] = want
				taken[want.Name] = true
				break
			}
		}
	}
	return renamed, nil
}

// renameIndexTargets returns the desired names renamed maps to.
func renameIndexTargets(renamed map[string]Index) map[string]string {
	targets := make(map[string]string, len(renamed))
	for from, to := range renamed {
		targets[to.Name] = from
	}
	return targets
}

// renameIndexChange is the statement that gives the live index from the
// desired index's name.
func renameIndexChange(schema, from, to string) Change {
	return Change{
		SQL:  "ALTER INDEX " + pgx.Identifier{schema, from}.Sanitize() + " RENAME TO " + pgx.Identifier{to}.Sanitize(),
		Kind: ChangeRenameIndex,
	}
}

func sortedIndexes(indexes []Index) []Index {
	sorted := slices.Clone(indexes)
	slices.SortFunc(sorted, func(a, b Index) int { return strings.Compare(a.Name, b.Name) })
	return sorted
}

// isUniqueIndex reports a unique standalone index.
func isUniqueIndex(ix Index) bool {
	return strings.HasPrefix(ix.Def, "CREATE UNIQUE INDEX ")
}

// isUniqueConstraint reports a unique constraint. A live key's IndexDef is
// not introspected, so it is told by the definition the server decompiles
// for it. A primary key is left out, since a table has only one, so a new
// primary key cannot be in place while the old one is.
func isUniqueConstraint(con Constraint) bool {
	return !con.Primary && strings.HasPrefix(con.Def, "UNIQUE ")
}

// heldDrops decides which of a plan's drops wait until the plan's new
// indexes and constraints are in place.
type heldDrops struct {
	// uniqueness says the plan builds a unique index or adds a unique
	// constraint, which may be what replaces a uniqueness it drops.
	uniqueness bool
	// renamedRules are the definitions of the constraints the desired table
	// adds under a name the live table does not have.
	renamedRules map[string]bool
	liveIdx      map[string]Index
	liveCons     map[string]Constraint
	desiredIdx   map[string]Index
	desiredCons  map[string]Constraint
}

// holds reports whether ch is a drop that waits: a unique index or unique
// constraint when the plan adds uniqueness, or a constraint the desired
// table adds again under another name. A drop whose name the desired table
// reuses never waits, since it clears that name.
func (h heldDrops) holds(ch Change) bool {
	if ch.Drops == nil || namedByDesired(ch.Drops.Name, h.desiredIdx, h.desiredCons) {
		return false
	}
	switch ch.Drops.Kind {
	case DroppedIndex:
		ix, ok := h.liveIdx[ch.Drops.Name]
		return ok && h.uniqueness && isUniqueIndex(ix)
	case DroppedConstraint:
		con, ok := h.liveCons[ch.Drops.Name]
		if !ok || con.Primary {
			return false
		}
		return (h.uniqueness && isUniqueConstraint(con)) || h.renamedRules[con.Def]
	}
	return false
}

// renamedConstraintDefs returns the definitions of the constraints desired
// adds under names the live table does not have. A live constraint the file
// no longer names whose definition is among them moved to another name.
func renamedConstraintDefs(desired Model, liveCons map[string]Constraint) map[string]bool {
	defs := make(map[string]bool)
	for _, con := range desired.Constraints {
		if _, ok := liveCons[con.Name]; !ok {
			defs[con.Def] = true
		}
	}
	return defs
}

// addsUniqueness reports whether the plan builds a unique index or adds a
// unique constraint, either of which may be what replaces a uniqueness the
// plan drops.
func addsUniqueness(desired Model, liveIdx map[string]Index, liveCons map[string]Constraint) bool {
	for _, ix := range desired.Indexes {
		if _, ok := liveIdx[ix.Name]; !ok && isUniqueIndex(ix) {
			return true
		}
	}
	for _, con := range desired.Constraints {
		if _, ok := liveCons[con.Name]; !ok && isUniqueConstraint(con) {
			return true
		}
	}
	return false
}

// namedByDesired reports whether the desired table names an index or a
// constraint name, which a drop has to clear before the plan creates it.
func namedByDesired(name string, desiredIdx map[string]Index, desiredCons map[string]Constraint) bool {
	if _, ok := desiredIdx[name]; ok {
		return true
	}
	_, ok := desiredCons[name]
	return ok
}
