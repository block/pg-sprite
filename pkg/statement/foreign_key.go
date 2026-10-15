package statement

import (
	"fmt"
	"slices"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// foreignKeyConstraints returns the CREATE TABLE's foreign-key constraints in
// both grammar forms: the table-constraint FOREIGN KEY ... REFERENCES and the
// column-constraint REFERENCES. This inspects constraint kinds only — no
// semantics are derived.
func foreignKeyConstraints(create *pganalyze.CreateStmt) []*pganalyze.Constraint {
	var fks []*pganalyze.Constraint
	for _, elt := range create.GetTableElts() {
		if con := elt.GetConstraint(); con != nil && con.GetContype() == pganalyze.ConstrType_CONSTR_FOREIGN {
			fks = append(fks, con)
		}
		for _, c := range elt.GetColumnDef().GetConstraints() {
			if con := c.GetConstraint(); con != nil && con.GetContype() == pganalyze.ConstrType_CONSTR_FOREIGN {
				fks = append(fks, con)
			}
		}
	}
	return fks
}

// referencedTables returns the distinct tables the CREATE TABLE's foreign
// keys reference, sorted, without the table itself: a self-reference binds
// to the table the desired file creates. A schema-qualified reference is
// refused with ErrQualifiedName for the same reason a qualified target is —
// desired files are schema-relative, and the scratch schema builds a
// referenced table under the target schema's name only.
func referencedTables(create *pganalyze.CreateStmt) ([]string, error) {
	table := create.GetRelation().GetRelname()
	var names []string
	for _, con := range foreignKeyConstraints(create) {
		ref := con.GetPktable()
		if ref.GetSchemaname() != "" {
			return nil, fmt.Errorf("%w: foreign key on %q references %s.%s",
				ErrQualifiedName, table, ref.GetSchemaname(), ref.GetRelname())
		}
		if ref.GetRelname() == table || slices.Contains(names, ref.GetRelname()) {
			continue
		}
		names = append(names, ref.GetRelname())
	}
	slices.Sort(names)
	return names, nil
}
