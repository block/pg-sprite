package schemadiff

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// A constraint whose definition changes under the same name is replaced in
// place: the old constraint keeps enforcing its rule until the statement
// that installs the new one also removes it. Dropping it first, in a step of
// its own, would leave the table without the rule for as long as the steps
// between take, and a concurrent index build waits for every older
// transaction in the database. Rows that break the old rule could land in
// that window, and a failed later step would leave the table without either
// rule.
//
// A primary key or unique constraint is replaced by building the new index
// concurrently under a temporary name, then dropping the old constraint and
// attaching the new index in one ALTER TABLE, which renames the index to the
// constraint's name. A failed build leaves the old constraint in place.
//
// A CHECK is replaced by dropping the old one and adding the new one NOT
// VALID in one ALTER TABLE, then validating it on its own: new writes are
// checked by one rule or the other at every moment, and the scan takes no
// lock that blocks them.
//
// A FOREIGN KEY keeps the drop and re-add, so the plan's foreign-key drop
// policy still sees the drop it makes.
//
// A unique constraint the desired table declares where the live table has a
// standalone index of the same name with another definition is built the
// same way: the new index is built under the temporary name while the old
// index keeps serving and, for a unique index, keeps rejecting duplicates;
// then the old index is dropped and the constraint takes over the new one.

// replacementIndexSuffix ends the temporary name a replaced key's new index
// is built under.
const replacementIndexSuffix = "_new"

// inPlaceReplacement is how one changed constraint is replaced in place.
type inPlaceReplacement struct {
	// built is set for a primary key or unique constraint whose new index
	// an earlier run already built under its temporary name, valid and with
	// the desired definition, and stopped before the swap. The plan then
	// swaps that index in without building it again.
	built bool
	// fromIndex is set for a unique constraint that takes over from a
	// standalone index of its name rather than from a constraint.
	fromIndex bool
	// index is that standalone index. Its zero value means an earlier run
	// already dropped it, after building the new index.
	index Index
}

// replacedInPlace names the constraints of a table diff that are replaced
// in place, keyed by constraint name. A partitioned parent keeps the drop
// and re-add, since the server refuses USING INDEX there. So does a table whose diff drops or retypes a column: a
// dropped column takes the constraints that read it with it, and a retype
// re-checks the constraints that are still in place.
//
// A key replacement an earlier run left after its build is picked up there:
// a valid index under the temporary name with the desired definition is
// swapped in, not built again. Any other index under that name is refused.
func replacedInPlace(partitioned, dropsOrRetypesColumn bool, schema string, liveCons map[string]Constraint, liveIdx map[string]Index, desired Model, desiredIdx map[string]Index) (map[string]inPlaceReplacement, error) {
	replaced := make(map[string]inPlaceReplacement)
	if partitioned || dropsOrRetypesColumn {
		return replaced, nil
	}
	desiredCons := constraintsByName(desired.Constraints)
	for _, con := range desired.Constraints {
		had, ok := liveCons[con.Name]
		if !ok && isUniqueKey(con) {
			r, swaps, err := indexToConstraint(schema, con, liveCons, liveIdx, desiredIdx, desiredCons)
			if err != nil {
				return nil, err
			}
			if swaps {
				replaced[con.Name] = r
			}
			continue
		}
		if !ok || had.Def == con.Def || validatesInPlace(had, con) || !sameConstraintFamily(had, con) {
			continue
		}
		if isKey(con) {
			if err := refuseReferencedKeyReplacement(had); err != nil {
				return nil, err
			}
			name := replacementIndexName(con.Name)
			if err := refuseTakenKeyReplacementName(con.Name, name, desiredIdx, desiredCons, liveCons); err != nil {
				return nil, err
			}
			if ix, taken := liveIdx[name]; taken {
				resumed, err := isKeyReplacementBuild(ix, con)
				if err != nil {
					return nil, err
				}
				if !resumed {
					return nil, replacementIndexTaken(schema, con.Name, ix)
				}
				replaced[con.Name] = inPlaceReplacement{built: true}
				continue
			}
		}
		replaced[con.Name] = inPlaceReplacement{}
	}
	return replaced, nil
}

// replacesPrimaryKey reports whether the plan replaces the table's primary
// key in place.
func replacesPrimaryKey(replaced map[string]inPlaceReplacement, liveCons map[string]Constraint) bool {
	for name := range replaced {
		if liveCons[name].Primary {
			return true
		}
	}
	return false
}

// indexToConstraint reports whether con, a unique constraint the live table
// does not have, replaces a standalone index of its name: a valid one whose
// definition differs from con's index, which a constraint cannot take over
// as it stands. One with con's index definition is adopted instead (see
// adoptableIndexes), and an unfinished build keeps the drop and the add. A
// replacement an earlier run left after dropping the old index is picked up
// there: a valid index under the temporary name with con's index definition
// that the desired table does not declare is taken over, not built again.
func indexToConstraint(schema string, con Constraint, liveCons map[string]Constraint, liveIdx, desiredIdx map[string]Index, desiredCons map[string]Constraint) (inPlaceReplacement, bool, error) {
	name := replacementIndexName(con.Name)
	ix, ok := liveIdx[con.Name]
	if !ok {
		built, taken := liveIdx[name]
		if _, declared := desiredIdx[name]; !taken || declared {
			return inPlaceReplacement{}, false, nil
		}
		resumed, err := isKeyReplacementBuild(built, con)
		if err != nil || !resumed {
			return inPlaceReplacement{}, false, err
		}
		return inPlaceReplacement{fromIndex: true, built: true}, true, nil
	}
	if ix.Invalid || ix.Def == con.IndexDef {
		return inPlaceReplacement{}, false, nil
	}
	if err := refuseReferencedIndexDrop(ix, true); err != nil {
		return inPlaceReplacement{}, false, err
	}
	if err := refuseTakenKeyReplacementName(con.Name, name, desiredIdx, desiredCons, liveCons); err != nil {
		return inPlaceReplacement{}, false, err
	}
	r := inPlaceReplacement{fromIndex: true, index: ix}
	if built, taken := liveIdx[name]; taken {
		resumed, err := isKeyReplacementBuild(built, con)
		if err != nil {
			return inPlaceReplacement{}, false, err
		}
		if !resumed {
			return inPlaceReplacement{}, false, replacementIndexTaken(schema, con.Name, built)
		}
		r.built = true
	}
	return r, true, nil
}

// refuseTakenKeyReplacementName refuses a key replacement whose temporary
// index name the desired table names, or a live constraint holds. A live
// index under it is handled by the caller, which can tell an earlier run's
// build from another index.
func refuseTakenKeyReplacementName(constraint, name string, desiredIdx map[string]Index, desiredCons, liveCons map[string]Constraint) error {
	if _, taken := desiredIdx[name]; taken {
		return unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the desired table also names; rename that index",
			ErrUnsupportedChange, constraint, name)
	}
	if _, taken := desiredCons[name]; taken {
		return unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the desired table also names; rename that constraint",
			ErrUnsupportedChange, constraint, name)
	}
	if _, taken := liveCons[name]; taken {
		return unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the live table already names; rename that constraint",
			ErrUnsupportedChange, constraint, name)
	}
	return nil
}

// refuseReferencedKeyReplacement refuses to replace a primary key or unique
// constraint that a foreign key references. PostgreSQL does not drop a key a
// foreign key depends on, so the swap would fail on every run after the new
// index was built, and leave that index on the table.
func refuseReferencedKeyReplacement(had Constraint) error {
	if len(had.ReferencedBy) == 0 {
		return nil
	}
	ref := had.ReferencedBy[0]
	return unsupported(CauseReferencedIndex, "%w: constraint %q is referenced by foreign key %q on table %s, and PostgreSQL does not drop a key a foreign key depends on; drop or change %q first, or keep the constraint's definition",
		ErrUnsupportedChange, had.Name, ref.Constraint, pgx.Identifier{ref.Schema, ref.Table}.Sanitize(), ref.Constraint)
}

// refuseReferencedKeyDrop refuses to drop a primary key or unique constraint
// that a foreign key still in place at that point in the plan references.
// PostgreSQL refuses the drop, so the plan would fail at execution after
// every statement ahead of it had committed. A foreign key on the same table
// that the plan has already dropped, named in dropped, no longer holds the
// key.
func refuseReferencedKeyDrop(schema, table string, con Constraint, dropped map[string]bool) error {
	for _, ref := range con.ReferencedBy {
		if ref.Schema == schema && ref.Table == table && dropped[ref.Constraint] {
			continue
		}
		return unsupported(CauseReferencedIndex, "%w: constraint %q is referenced by foreign key %q on table %s, and PostgreSQL does not drop a key a foreign key depends on; drop or change %q first, or keep the constraint",
			ErrUnsupportedChange, con.Name, ref.Constraint, pgx.Identifier{ref.Schema, ref.Table}.Sanitize(), ref.Constraint)
	}
	return nil
}

// isKeyReplacementBuild reports whether ix, the live index under a key
// replacement's temporary name, is an earlier run's finished build of con's
// new index: valid and with con's index definition.
func isKeyReplacementBuild(ix Index, con Constraint) (bool, error) {
	if ix.Invalid {
		return false, nil
	}
	return sameIndexUnderName(ix, Index{Name: con.Name, Def: con.IndexDef})
}

// replacementIndexTaken refuses a replacement whose temporary index name a
// live index already holds. An invalid one is most likely an earlier
// attempt's own build, which a failed build leaves behind; the refusal names
// the statement that clears it, since no plan can tell an abandoned build from
// one still running. A valid one the desired table does not declare (a
// declared one is refused before this) is most likely an earlier attempt's
// build for another definition, which the file no longer asks for either; the
// refusal names the same statement, since the replacement must not drop an
// index it did not build and reuse its name.
func replacementIndexTaken(schema, constraint string, ix Index) error {
	drop := pgx.Identifier{schema, ix.Name}.Sanitize()
	if ix.Invalid {
		return unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, and an unfinished build of that index is on the table, likely left by an earlier attempt; once pg_stat_progress_create_index shows no backend building it, run DROP INDEX CONCURRENTLY %s and plan again",
			ErrUnsupportedChange, constraint, ix.Name, drop)
	}
	return unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, and a valid index the desired table does not declare already holds that name, likely an earlier attempt's build for another definition; run DROP INDEX CONCURRENTLY %s and plan again",
		ErrUnsupportedChange, constraint, ix.Name, drop)
}

// isKey reports a primary key or unique constraint: one that owns an index.
func isKey(con Constraint) bool {
	return con.IndexDef != ""
}

// isUniqueKey reports a desired unique constraint. A primary key is left
// out: its columns are NOT NULL, which the table may not have yet, and a
// table has only one.
func isUniqueKey(con Constraint) bool {
	return isKey(con) && !con.Primary
}

// isRule reports a CHECK or FOREIGN KEY: a constraint the server can add NOT
// VALID and validate apart.
func isRule(con Constraint) bool {
	return addedNotValid(con) != nil
}

// sameConstraintFamily reports whether desired can replace live in one
// statement: both are primary keys, both are unique constraints, or both
// are CHECKs. A live key's IndexDef is not introspected, so
// a live unique constraint is told apart from an exclusion constraint by the
// definition the server decompiles for it.
func sameConstraintFamily(live, desired Constraint) bool {
	switch {
	case isKey(desired) && desired.Primary:
		return live.Primary
	case isKey(desired):
		return !live.Primary && strings.HasPrefix(live.Def, "UNIQUE ")
	case isRule(desired) && !desired.ForeignKey:
		return isRule(live) && !live.ForeignKey
	}
	return false
}

// replacementIndexName is the temporary name a replaced key's new index is
// built under: the constraint's name with replacementIndexSuffix, cut to
// fit PostgreSQL's identifier limit with a digest of the full name so two
// long names never cut to the same one.
func replacementIndexName(constraint string) string {
	name := constraint + replacementIndexSuffix
	if len(name) <= maxIdentifierBytes {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := fmt.Sprintf("_%x%s", sum[:4], replacementIndexSuffix)
	head := constraint[:maxIdentifierBytes-len(suffix)]
	for !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	return head + suffix
}

// replaceInPlaceChanges is the plan that replaces had with con in place.
func replaceInPlaceChanges(table, schema string, had, con Constraint, r inPlaceReplacement) ([]Change, error) {
	if r.fromIndex {
		return replaceIndexWithKeyChanges(table, schema, con, r)
	}
	if isKey(con) {
		return replaceKeyChanges(table, schema, had, con, r.built)
	}
	return replaceRuleChanges(table, had, con), nil
}

// dropsOrRetypesColumn reports whether the diff drops a live column or
// changes a kept column's type.
func dropsOrRetypesColumn(live Model, desiredCols map[string]Column) bool {
	for _, col := range live.Columns {
		want, ok := desiredCols[col.Name]
		if !ok || want.Type != col.Type {
			return true
		}
	}
	return false
}

// replaceKeyChanges builds con's new index under its temporary name, then
// drops had and attaches the index as con in one statement. built leaves the
// build out, for an index an earlier run already built.
func replaceKeyChanges(table, schema string, had, con Constraint, built bool) ([]Change, error) {
	index := replacementIndexName(con.Name)
	def, err := statement.RenameIndex(con.IndexDef, index)
	if err != nil {
		return nil, fmt.Errorf("name the replacement index of constraint %s: %w", con.Name, err)
	}
	var changes []Change
	if !built {
		create, err := statement.Qualify(def, schema)
		if err != nil {
			return nil, fmt.Errorf("qualify the replacement index of constraint %s: %w", con.Name, err)
		}
		changes = append(changes, Change{SQL: create, Kind: ChangeCreateIndex})
	}
	return append(changes,
		Change{
			SQL:         "ALTER TABLE " + table + " DROP CONSTRAINT " + pgx.Identifier{had.Name}.Sanitize() + ", " + addUsingIndexClause(con, index),
			Kind:        ChangeAddConstraint,
			Destructive: true,
			Adopts:      &IndexAdoption{Index: index, Def: def},
			Drops:       &DroppedObject{Kind: DroppedConstraint, Name: had.Name, Definition: had.Def},
		},
	), nil
}

// replaceIndexWithKeyChanges builds con's index under its temporary name,
// drops the standalone index r.index once that build is valid, then
// attaches the new index as con, which renames it to con's name. Each step
// a resumed replacement has already taken is left out.
func replaceIndexWithKeyChanges(table, schema string, con Constraint, r inPlaceReplacement) ([]Change, error) {
	index := replacementIndexName(con.Name)
	def, err := statement.RenameIndex(con.IndexDef, index)
	if err != nil {
		return nil, fmt.Errorf("name the replacement index of constraint %s: %w", con.Name, err)
	}
	var changes []Change
	if !r.built {
		create, err := statement.Qualify(def, schema)
		if err != nil {
			return nil, fmt.Errorf("qualify the replacement index of constraint %s: %w", con.Name, err)
		}
		changes = append(changes, Change{SQL: create, Kind: ChangeCreateIndex})
	}
	if r.index.Name != "" {
		changes = append(changes, Change{
			SQL:         "DROP INDEX " + pgx.Identifier{schema, r.index.Name}.Sanitize(),
			Kind:        ChangeDropIndex,
			Destructive: true,
			Drops:       &DroppedObject{Kind: DroppedIndex, Name: r.index.Name, Definition: r.index.Def},
			ReplacedBy:  index,
			Replacement: &IndexReplacement{Index: index, Def: def},
		})
	}
	return append(changes, Change{
		SQL:    "ALTER TABLE " + table + " " + addUsingIndexClause(con, index),
		Kind:   ChangeAddConstraint,
		Adopts: &IndexAdoption{Index: index, Def: def},
	}), nil
}

// replaceRuleChanges drops had and adds con NOT VALID in one statement, then
// validates con on its own.
func replaceRuleChanges(table string, had, con Constraint) []Change {
	name := pgx.Identifier{con.Name}.Sanitize()
	sql := "ALTER TABLE " + table + " DROP CONSTRAINT " + pgx.Identifier{had.Name}.Sanitize() +
		", ADD CONSTRAINT " + name + " " + con.Def + notValidSuffix
	return []Change{
		{
			SQL:         sql,
			Kind:        addConstraintKind(con),
			Destructive: true,
			Drops:       &DroppedObject{Kind: DroppedConstraint, Name: had.Name, Definition: had.Def},
		},
		{
			SQL:       "ALTER TABLE " + table + " VALIDATE CONSTRAINT " + name,
			Kind:      validateConstraintKind(con),
			Validates: addedNotValid(con),
		},
	}
}
