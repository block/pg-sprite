package statement

import (
	"fmt"
	"slices"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
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

// QualifyForeignKeyReferences returns an ALTER TABLE statement with every
// unqualified table its added foreign keys reference qualified by schema. A
// planned foreign-key add names its referenced table as the desired file
// does, unqualified, which resolves through the search_path of whatever
// session runs it; the referenced table lives in the target schema, so the
// plan names it there and the statement binds the same table wherever it
// runs. An already-qualified reference is left as written. Like Qualify,
// this touches qualification only.
func QualifyForeignKeyReferences(sql, schema string) (string, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("parse statement to qualify foreign-key references: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return "", fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	node := tree.GetStmts()[0].GetStmt()
	alter := node.GetAlterTableStmt()
	if alter == nil {
		return "", ErrDisallowedStatement
	}
	for _, c := range alter.GetCmds() {
		cmd := c.GetAlterTableCmd()
		if cmd.GetSubtype() != pganalyze.AlterTableType_AT_AddConstraint {
			continue
		}
		con := cmd.GetDef().GetConstraint()
		if con.GetContype() != pganalyze.ConstrType_CONSTR_FOREIGN || con.GetPktable() == nil {
			continue
		}
		if con.GetPktable().GetSchemaname() == "" {
			con.GetPktable().Schemaname = schema
		}
	}
	return deparseOne(node)
}
