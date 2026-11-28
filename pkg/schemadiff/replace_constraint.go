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
// A CHECK or FOREIGN KEY is replaced by dropping the old one and adding the
// new one NOT VALID in one ALTER TABLE, then validating it on its own: new
// writes are checked by one rule or the other at every moment, and the scan
// takes no lock that blocks them. The statement is destructive and names the
// constraint it drops, so a review of the plan sees the drop. A VALIDATE that
// fails leaves the new rule NOT VALID, still checking every new write, and
// the next plan validates it again once the rows are fixed.

// replacementIndexSuffix ends the temporary name a replaced key's new index
// is built under.
const replacementIndexSuffix = "_new"

// inPlaceReplacement is how one changed constraint is replaced in place.
type inPlaceReplacement struct {
	// live is the live constraint the replacement drops: the one under the
	// same name, or the primary key a renamed primary key replaces.
	live Constraint
	// built is set for a primary key or unique constraint whose new index
	// an earlier run already built under its temporary name, valid and with
	// the desired definition, and stopped before the swap. The plan then
	// swaps that index in without building it again.
	built bool
}

// replacedInPlace names the constraints of a table diff that are replaced
// in place, keyed by constraint name. On a partitioned parent only a CHECK
// is: the server takes a NOT VALID CHECK there, adding it to every
// partition, but refuses USING INDEX and NOT VALID foreign keys, so a
// changed key or foreign key keeps the drop and re-add. A table whose diff
// changes its columns first (see changesColumnsFirst) keeps it for every
// constraint. A diff that only drops columns replaces in place
// like any other: the column drops run after the replacements, so a dropped
// column never takes an old constraint with it before the new one holds.
//
// A primary key the file declares under another name is replaced the same
// way, since a table has only one: dropping the old key first would let
// duplicates in while the new key's index builds, and a failed build would
// leave the table with no key at all.
//
// A key replacement an earlier run left after its build is picked up there:
// a valid index under the temporary name with the desired definition is
// swapped in, not built again. Any other index under that name is refused.
func replacedInPlace(partitioned, columnsFirst bool, schema string, liveCons map[string]Constraint, liveIdx map[string]Index, desired Model, desiredIdx map[string]Index) (map[string]inPlaceReplacement, error) {
	replaced := make(map[string]inPlaceReplacement)
	if columnsFirst {
		return replaced, nil
	}
	desiredCons := constraintsByName(desired.Constraints)
	for _, con := range desired.Constraints {
		had, ok := liveCons[con.Name]
		if !ok || had.Def == con.Def || validatesInPlace(had, con) || !sameConstraintFamily(had, con) {
			continue
		}
		if partitioned && !isCheckConstraint(con) {
			continue
		}
		if !isKey(con) {
			replaced[con.Name] = inPlaceReplacement{live: had}
			continue
		}
		r, err := keyReplacement(schema, had, con, liveCons, liveIdx, desiredIdx, desiredCons)
		if err != nil {
			return nil, err
		}
		replaced[con.Name] = r
	}
	if partitioned {
		return replaced, nil
	}
	if had, con, ok := renamedPrimaryKey(liveCons, liveIdx, desired.Constraints, desiredCons); ok {
		r, err := keyReplacement(schema, had, con, liveCons, liveIdx, desiredIdx, desiredCons)
		if err != nil {
			return nil, err
		}
		replaced[con.Name] = r
	}
	return replaced, nil
}

// keyReplacement checks that con's new index can be built under its
// temporary name to replace had, and picks up a build an earlier run left
// there.
func keyReplacement(schema string, had, con Constraint, liveCons map[string]Constraint, liveIdx map[string]Index, desiredIdx map[string]Index, desiredCons map[string]Constraint) (inPlaceReplacement, error) {
	if err := refuseReferencedKeyReplacement(had); err != nil {
		return inPlaceReplacement{}, err
	}
	name := replacementIndexName(con.Name)
	if _, taken := desiredIdx[name]; taken {
		return inPlaceReplacement{}, unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the desired table also names; rename that index",
			ErrUnsupportedChange, con.Name, name)
	}
	if _, taken := desiredCons[name]; taken {
		return inPlaceReplacement{}, unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the desired table also names; rename that constraint",
			ErrUnsupportedChange, con.Name, name)
	}
	if _, taken := liveCons[name]; taken {
		return inPlaceReplacement{}, unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the live table already names; rename that constraint",
			ErrUnsupportedChange, con.Name, name)
	}
	if ix, taken := liveIdx[name]; taken {
		resumed, err := isKeyReplacementBuild(ix, con)
		if err != nil {
			return inPlaceReplacement{}, err
		}
		if !resumed {
			return inPlaceReplacement{}, replacementIndexTaken(schema, con.Name, ix)
		}
		return inPlaceReplacement{live: had, built: true}, nil
	}
	return inPlaceReplacement{live: had}, nil
}

// renamedPrimaryKey returns the live primary key and the desired one when
// the file declares the table's primary key under a name the live table
// does not have, and the live key's name is not declared at all. A desired
// key whose name a live standalone index has is left to be adopted.
func renamedPrimaryKey(liveCons map[string]Constraint, liveIdx map[string]Index, desired []Constraint, desiredCons map[string]Constraint) (had, con Constraint, ok bool) {
	for _, c := range desired {
		if c.Primary && isKey(c) {
			con, ok = c, true
		}
	}
	if !ok {
		return Constraint{}, Constraint{}, false
	}
	if _, named := liveCons[con.Name]; named {
		return Constraint{}, Constraint{}, false
	}
	if _, named := liveIdx[con.Name]; named {
		return Constraint{}, Constraint{}, false
	}
	for _, c := range liveCons {
		if !c.Primary {
			continue
		}
		if _, named := desiredCons[c.Name]; named {
			return Constraint{}, Constraint{}, false
		}
		return c, con, true
	}
	return Constraint{}, Constraint{}, false
}

// replacedLiveConstraints names the live constraints the plan replaces in
// place, which the statement that adds each replacement drops.
func replacedLiveConstraints(replaced map[string]inPlaceReplacement) map[string]bool {
	names := make(map[string]bool, len(replaced))
	for _, r := range replaced {
		names[r.live.Name] = true
	}
	return names
}

// replacesPrimaryKey reports whether the plan replaces the table's primary
// key in place.
func replacesPrimaryKey(replaced map[string]inPlaceReplacement) bool {
	for _, r := range replaced {
		if r.live.Primary {
			return true
		}
	}
	return false
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

// isRule reports a CHECK or FOREIGN KEY: a constraint the server can add NOT
// VALID and validate apart.
func isRule(con Constraint) bool {
	return addedNotValid(con) != nil
}

// sameConstraintFamily reports whether desired can replace live in one
// statement: both are primary keys, both are unique constraints, both are
// CHECKs, or both are foreign keys. A live key's IndexDef is not introspected, so
// a live unique constraint is told apart from an exclusion constraint by the
// definition the server decompiles for it.
func sameConstraintFamily(live, desired Constraint) bool {
	switch {
	case isKey(desired) && desired.Primary:
		return live.Primary
	case isKey(desired):
		return !live.Primary && strings.HasPrefix(live.Def, "UNIQUE ")
	case isRule(desired):
		return isRule(live) && live.ForeignKey == desired.ForeignKey
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
	if isKey(con) {
		return replaceKeyChanges(table, schema, had, con, r.built)
	}
	return replaceRuleChanges(table, schema, had, con)
}

// changesColumnsFirst reports whether the diff runs its column drops and
// retypes ahead of everything it builds, rather than holding the column
// drops for the end of the plan. A retype does, since it re-checks and
// rebuilds the indexes and constraints still in place. So does a column drop
// beside an added column that may rewrite the table: a rewrite renumbers the
// columns, and a dropped column is reviewed by the number it has when the
// plan is derived. An added column may rewrite when it carries a default, a
// generation expression, or an identity, since only the planner can prove
// such an add runs in place.
func changesColumnsFirst(live Model, desired []Column) bool {
	liveCols := columnsByName(live.Columns)
	desiredCols := columnsByName(desired)
	for _, col := range live.Columns {
		if want, ok := desiredCols[col.Name]; ok && want.Type != col.Type {
			return true
		}
	}
	return dropsColumn(live, desiredCols) && addsColumnThatMayRewrite(liveCols, desired)
}

// dropsColumn reports whether the diff drops a live column.
func dropsColumn(live Model, desiredCols map[string]Column) bool {
	for _, col := range live.Columns {
		if _, ok := desiredCols[col.Name]; !ok {
			return true
		}
	}
	return false
}

// addsColumnThatMayRewrite reports whether the diff adds a column whose
// add the planner may route to a table rewrite.
func addsColumnThatMayRewrite(liveCols map[string]Column, desired []Column) bool {
	for _, col := range desired {
		if _, ok := liveCols[col.Name]; ok {
			continue
		}
		if col.Default != "" || col.Generated || col.Identity != IdentityNone {
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

// replaceRuleChanges drops had and adds con NOT VALID in one statement, then
// validates con on its own.
func replaceRuleChanges(table, schema string, had, con Constraint) ([]Change, error) {
	name := pgx.Identifier{con.Name}.Sanitize()
	sql := "ALTER TABLE " + table + " DROP CONSTRAINT " + pgx.Identifier{had.Name}.Sanitize() +
		", ADD CONSTRAINT " + name + " " + con.Def + notValidSuffix
	var references *ForeignKeyReference
	if con.ForeignKey {
		qualified, err := statement.QualifyForeignKeyReferences(sql, schema)
		if err != nil {
			return nil, fmt.Errorf("qualify foreign key %s: %w", con.Name, err)
		}
		sql = qualified
		references = con.References
	}
	return []Change{
		{
			SQL:         sql,
			Kind:        addConstraintKind(con),
			Destructive: true,
			References:  references,
			Drops:       &DroppedObject{Kind: DroppedConstraint, Name: had.Name, Definition: had.Def},
		},
		{
			SQL:       "ALTER TABLE " + table + " VALIDATE CONSTRAINT " + name,
			Kind:      validateConstraintKind(con),
			Validates: addedNotValid(con),
		},
	}, nil
}
