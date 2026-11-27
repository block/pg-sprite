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

// replacementIndexSuffix ends the temporary name a replaced key's new index
// is built under.
const replacementIndexSuffix = "_new"

// replacedInPlace names the constraints of a table diff that are replaced
// in place, keyed by constraint name. A partitioned parent keeps the drop
// and re-add, since the server refuses USING INDEX there. So does a table whose diff drops or retypes a column: a
// dropped column takes the constraints that read it with it, and a retype
// re-checks the constraints that are still in place. A primary key keeps
// them too on a table whose diff drops a NOT NULL: the key owns its columns'
// NOT NULL until it is dropped, and the plan drops NOT NULLs before it adds
// constraints.
func replacedInPlace(partitioned, dropsOrRetypesColumn, dropsNotNull bool, schema string, liveCons map[string]Constraint, liveIdx map[string]Index, desired Model, desiredIdx map[string]Index) (map[string]struct{}, error) {
	replaced := make(map[string]struct{})
	if partitioned || dropsOrRetypesColumn {
		return replaced, nil
	}
	desiredCons := constraintsByName(desired.Constraints)
	for _, con := range desired.Constraints {
		had, ok := liveCons[con.Name]
		if !ok || had.Def == con.Def || validatesInPlace(had, con) || !sameConstraintFamily(had, con) {
			continue
		}
		if con.Primary && dropsNotNull {
			continue
		}
		if isKey(con) {
			name := replacementIndexName(con.Name)
			if _, taken := desiredIdx[name]; taken {
				return nil, unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the desired table also names; rename that index",
					ErrUnsupportedChange, con.Name, name)
			}
			if _, taken := desiredCons[name]; taken {
				return nil, unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the desired table also names; rename that constraint",
					ErrUnsupportedChange, con.Name, name)
			}
			if _, taken := liveCons[name]; taken {
				return nil, unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which the live table already names; rename that constraint",
					ErrUnsupportedChange, con.Name, name)
			}
			if ix, taken := liveIdx[name]; taken {
				return nil, replacementIndexTaken(schema, con.Name, ix)
			}
		}
		replaced[con.Name] = struct{}{}
	}
	return replaced, nil
}

// replacementIndexTaken refuses a replacement whose temporary index name a
// live index already holds. An invalid one is most likely an earlier
// attempt's own build, which a failed build leaves behind; the refusal names
// the statement that clears it, since no plan can tell an abandoned build from
// one still running. A valid one is either an earlier attempt's finished
// build, whose swap did not run, or an index of the table's own. The first
// already enforces the new rule, so renaming it would keep that rule on the
// table under another name: the refusal says to drop it in that case, and to
// rename it only when it is the table's own.
func replacementIndexTaken(schema, constraint string, ix Index) error {
	drop := pgx.Identifier{schema, ix.Name}.Sanitize()
	if ix.Invalid {
		return unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, and an unfinished build of that index is on the table, likely left by an earlier attempt; once pg_stat_progress_create_index shows no backend building it, run DROP INDEX CONCURRENTLY %s and plan again",
			ErrUnsupportedChange, constraint, ix.Name, drop)
	}
	return unsupported(CauseReplacementIndexName, "%w: constraint %q is replaced by building its new index as %q, which a live index on the table already names; if an earlier attempt built it and its swap did not run, run DROP INDEX CONCURRENTLY %s and plan again, and if the index is the table's own, rename it",
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
func replaceInPlaceChanges(table, schema string, had, con Constraint) ([]Change, error) {
	if isKey(con) {
		return replaceKeyChanges(table, schema, had, con)
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
// drops had and attaches the index as con in one statement.
func replaceKeyChanges(table, schema string, had, con Constraint) ([]Change, error) {
	index := replacementIndexName(con.Name)
	def, err := statement.RenameIndex(con.IndexDef, index)
	if err != nil {
		return nil, fmt.Errorf("name the replacement index of constraint %s: %w", con.Name, err)
	}
	create, err := statement.Qualify(def, schema)
	if err != nil {
		return nil, fmt.Errorf("qualify the replacement index of constraint %s: %w", con.Name, err)
	}
	return []Change{
		{SQL: create, Kind: ChangeCreateIndex},
		{
			SQL:         "ALTER TABLE " + table + " DROP CONSTRAINT " + pgx.Identifier{had.Name}.Sanitize() + ", " + addUsingIndexClause(con, index),
			Kind:        ChangeAddConstraint,
			Destructive: true,
			Adopts:      &IndexAdoption{Index: index, Def: def},
			Drops:       &DroppedObject{Kind: DroppedConstraint, Name: had.Name, Definition: had.Def},
		},
	}, nil
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
