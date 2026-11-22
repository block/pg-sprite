package schemadiff

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/block/pg-sprite/pkg/statement"
)

// maxIdentifierBytes is PostgreSQL's NAMEDATALEN - 1: the longest name, in
// bytes, the server keeps without truncating it.
const maxIdentifierBytes = 63

// generatedCheckLabel matches the label the server ends a check
// constraint's generated name with: `check`, then the pass number it moved
// on to when an earlier choice was taken.
var generatedCheckLabel = regexp.MustCompile(`check[0-9]*$`)

// markGeneratedCheckNames flags each check constraint of a desired model
// that none of the desired statements names. The server named it on the
// scratch table, in the order the file declares it, so the name is an
// accident of that order and not something the file asks for. When a
// statement can bring in constraints under names it does not state (a LIKE
// clause that copies constraints), nothing is flagged and the diff matches
// every constraint by name as before.
func markGeneratedCheckNames(statements []statement.Statement, m *Model) error {
	stated := make(map[string]bool)
	for _, st := range statements {
		if st.Kind() != statement.KindCreateTable && st.Kind() != statement.KindAlterTable {
			continue
		}
		names, err := statement.ConstraintNames(st.SQL())
		if err != nil {
			return fmt.Errorf("read constraint names of desired statement: %w", err)
		}
		if !names.Complete {
			return nil
		}
		for _, name := range names.Names {
			stated[name] = true
		}
	}
	for i, c := range m.Constraints {
		if isCheckConstraint(c) && !stated[c.Name] {
			m.Constraints[i].GeneratedName = true
		}
	}
	return nil
}

// matchGeneratedCheckNames renames each desired check constraint the file
// leaves unnamed to the live check constraint with the same definition, so
// the name-keyed diff sees no change where only the server's naming order
// differs. PostgreSQL numbers unnamed check constraints in creation order
// (`<table>_check`, `<table>_check1`, ...), so a table built by a history of
// statements and the same table built from one file can hold identical
// checks under shifted names.
//
// A desired check keeps the live constraint of its own name when the two
// definitions match. Otherwise it takes the first unclaimed live check, in
// live order, with its definition. A live constraint whose name the file
// states is never claimed: that declaration owns it. A desired check that
// matches nothing keeps its generated name and is added under it, unless a
// matched check now holds that name; then it moves on to the next free
// pass number, as the server would, so the add never collides with a
// constraint the plan keeps.
func matchGeneratedCheckNames(live, desired Model) (Model, error) {
	if !slices.ContainsFunc(desired.Constraints, func(c Constraint) bool { return c.GeneratedName }) {
		return desired, nil
	}
	desired.Constraints = slices.Clone(desired.Constraints)
	stated := make(map[string]bool)
	for _, c := range desired.Constraints {
		if !c.GeneratedName {
			stated[c.Name] = true
		}
	}
	liveByName := constraintsByName(live.Constraints)
	claimed := make(map[string]bool)
	matched := make([]bool, len(desired.Constraints))
	for i, c := range desired.Constraints {
		if !c.GeneratedName {
			continue
		}
		if l, ok := liveByName[c.Name]; ok && sameCheck(l, c) {
			claimed[l.Name] = true
			matched[i] = true
		}
	}
	for i, c := range desired.Constraints {
		if !c.GeneratedName || matched[i] {
			continue
		}
		for _, l := range live.Constraints {
			if claimed[l.Name] || stated[l.Name] || !sameCheck(l, c) {
				continue
			}
			claimed[l.Name] = true
			matched[i] = true
			desired.Constraints[i].Name = l.Name
			break
		}
	}
	taken := make(map[string]bool, len(live.Constraints)+len(desired.Constraints))
	for _, c := range live.Constraints {
		taken[c.Name] = true
	}
	for _, c := range desired.Constraints {
		taken[c.Name] = true
	}
	for i, c := range desired.Constraints {
		if !c.GeneratedName || matched[i] || !heldByMatch(desired.Constraints, matched, i) {
			continue
		}
		name, err := nextGeneratedCheckName(c.Name, taken)
		if err != nil {
			return Model{}, err
		}
		taken[name] = true
		desired.Constraints[i].Name = name
	}
	return desired, nil
}

// heldByMatch reports whether a matched desired constraint other than
// cons[i] now holds cons[i]'s name.
func heldByMatch(cons []Constraint, matched []bool, i int) bool {
	for j, c := range cons {
		if j != i && matched[j] && c.Name == cons[i].Name {
			return true
		}
	}
	return false
}

// nextGeneratedCheckName returns the first name after name, in the server's
// numbering of a check constraint's generated name, that taken does not
// hold. A name the server truncated to fit the identifier budget would need
// its table and column parts shortened again for a longer pass number,
// which the numbering here does not do, so a candidate over the budget is
// refused rather than guessed.
func nextGeneratedCheckName(name string, taken map[string]bool) (string, error) {
	loc := generatedCheckLabel.FindStringIndex(name)
	if loc == nil {
		return "", fmt.Errorf("%w: check constraint %q does not end in a generated label", ErrUnsupportedChange, name)
	}
	prefix := name[:loc[0]] + "check"
	for pass := 1; ; pass++ {
		candidate := prefix + strconv.Itoa(pass)
		if len(candidate) > maxIdentifierBytes {
			return "", fmt.Errorf("%w: no free generated name for check constraint %q within %d bytes", ErrUnsupportedChange, name, maxIdentifierBytes)
		}
		if !taken[candidate] {
			return candidate, nil
		}
	}
}

// sameCheck reports whether a live constraint is a check constraint with
// the desired check's definition, as printed or as one of its replays. A
// live check left NOT VALID matches on its definition without the suffix,
// so the diff validates it instead of dropping and re-adding it.
func sameCheck(live, desired Constraint) bool {
	if !isCheckConstraint(live) {
		return false
	}
	def := liveDefWithoutNotValid(live)
	return def == desired.Def || slices.Contains(desired.DefReplays, def)
}

// isCheckConstraint reports a CHECK constraint, by the definition the
// server prints for it.
func isCheckConstraint(c Constraint) bool {
	return !c.ForeignKey && strings.HasPrefix(c.Def, "CHECK ")
}
