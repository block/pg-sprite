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

// ForeignKeyClause is what an ADD CONSTRAINT ... FOREIGN KEY declares, as
// the statement spells it. Match, OnUpdate, and OnDelete hold the one-letter
// codes the grammar and pg_constraint share, with the grammar's defaults
// filled in when the statement says nothing: Match is "s" (SIMPLE), "f"
// (FULL), or "p" (PARTIAL); an action is "a" (NO ACTION), "r" (RESTRICT),
// "c" (CASCADE), "n" (SET NULL), or "d" (SET DEFAULT).
type ForeignKeyClause struct {
	// Columns are the referencing columns, in order.
	Columns []string
	// RefSchema and RefTable name the referenced table; RefSchema is empty
	// when the statement does not qualify it.
	RefSchema string
	RefTable  string
	// RefColumns are the referenced columns, in order; empty when the
	// statement leaves them to the referenced table's primary key.
	RefColumns []string
	Match      string
	OnUpdate   string
	OnDelete   string
	// DeleteSetColumns are the columns of an ON DELETE SET NULL (...) or
	// SET DEFAULT (...) column list; empty when there is none.
	DeleteSetColumns []string
	// Deferrable and InitiallyDeferred are the constraint's DEFERRABLE and
	// INITIALLY DEFERRED.
	Deferrable        bool
	InitiallyDeferred bool
}

// foreignKeyClause reads the FOREIGN KEY's columns, referenced side,
// actions, and deferrability.
func foreignKeyClause(con *pganalyze.Constraint) *ForeignKeyClause {
	fk := &ForeignKeyClause{
		RefSchema:         con.GetPktable().GetSchemaname(),
		RefTable:          con.GetPktable().GetRelname(),
		Match:             con.GetFkMatchtype(),
		OnUpdate:          con.GetFkUpdAction(),
		OnDelete:          con.GetFkDelAction(),
		Deferrable:        con.GetDeferrable(),
		InitiallyDeferred: con.GetInitdeferred(),
	}
	for _, n := range con.GetFkAttrs() {
		fk.Columns = append(fk.Columns, n.GetString_().GetSval())
	}
	for _, n := range con.GetPkAttrs() {
		fk.RefColumns = append(fk.RefColumns, n.GetString_().GetSval())
	}
	for _, n := range con.GetFkDelSetCols() {
		fk.DeleteSetColumns = append(fk.DeleteSetColumns, n.GetString_().GetSval())
	}
	return fk
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

// ForeignKeyReferencedTables returns the tables the foreign keys a CREATE
// TABLE declares, or an ALTER TABLE adds, reference, each as the statement
// spells it, in statement order without repeats, and none for any other
// statement. Both grammar forms count: a table constraint FOREIGN KEY and a
// column's inline REFERENCES. Creating or adding a foreign key locks the
// referenced table as well as the table it is on.
func ForeignKeyReferencedTables(sql string) ([]TableName, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return nil, err
	}
	var tables []TableName
	add := func(con *pganalyze.Constraint) {
		if con == nil || con.GetContype() != pganalyze.ConstrType_CONSTR_FOREIGN || con.GetPktable() == nil {
			return
		}
		ref := TableName{Schema: con.GetPktable().GetSchemaname(), Name: con.GetPktable().GetRelname()}
		if !slices.Contains(tables, ref) {
			tables = append(tables, ref)
		}
	}
	if create := node.GetCreateStmt(); create != nil {
		for _, con := range foreignKeyConstraints(create) {
			add(con)
		}
		return tables, nil
	}
	for _, c := range node.GetAlterTableStmt().GetCmds() {
		cmd := c.GetAlterTableCmd()
		switch cmd.GetSubtype() {
		case pganalyze.AlterTableType_AT_AddConstraint:
			add(cmd.GetDef().GetConstraint())
		case pganalyze.AlterTableType_AT_AddColumn:
			for _, inline := range cmd.GetDef().GetColumnDef().GetConstraints() {
				add(inline.GetConstraint())
			}
		}
	}
	return tables, nil
}
