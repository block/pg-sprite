package statement

import (
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// likeIncludingConstraints is the bit a LIKE clause's options set when the
// clause copies CHECK constraints, CREATE_TABLE_LIKE_CONSTRAINTS in
// PostgreSQL's parsenodes.h. INCLUDING ALL sets it, and EXCLUDING
// CONSTRAINTS clears it.
const likeIncludingConstraints = 1 << 2

// StatedConstraintNames are the constraint names one statement states.
type StatedConstraintNames struct {
	// Names holds every name the statement states for a constraint it
	// declares, table-level and column-inline, of every constraint type,
	// in definition order. A constraint the statement declares without a
	// name takes one the server chooses and is not listed.
	Names []string
	// Complete is false when the statement can bring in constraints under
	// names it does not state: a CREATE TABLE whose LIKE clause copies
	// another table's constraints. A constraint the statement builds is
	// then not known to be unnamed merely because Names leaves it out.
	Complete bool
}

// ConstraintNames returns the constraint names a CREATE TABLE or a
// single-command ALTER TABLE ... ADD CONSTRAINT states. Any other statement
// declares no constraint and returns an empty, complete result.
func ConstraintNames(sql string) (StatedConstraintNames, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return StatedConstraintNames{}, fmt.Errorf("parse statement: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return StatedConstraintNames{}, fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	stated := StatedConstraintNames{Complete: true}
	add := func(con *pganalyze.Constraint) {
		if con != nil && con.GetConname() != "" {
			stated.Names = append(stated.Names, con.GetConname())
		}
	}
	stmt := tree.GetStmts()[0].GetStmt()
	if create := stmt.GetCreateStmt(); create != nil {
		for _, elt := range create.GetTableElts() {
			if like := elt.GetTableLikeClause(); like != nil {
				if like.GetOptions()&likeIncludingConstraints != 0 {
					stated.Complete = false
				}
				continue
			}
			if con := elt.GetConstraint(); con != nil {
				add(con)
				continue
			}
			for _, c := range elt.GetColumnDef().GetConstraints() {
				add(c.GetConstraint())
			}
		}
		return stated, nil
	}
	if alter := stmt.GetAlterTableStmt(); alter != nil {
		for _, cmd := range alter.GetCmds() {
			at := cmd.GetAlterTableCmd()
			switch at.GetSubtype() {
			case pganalyze.AlterTableType_AT_AddConstraint:
				add(at.GetDef().GetConstraint())
			case pganalyze.AlterTableType_AT_AddColumn:
				for _, c := range at.GetDef().GetColumnDef().GetConstraints() {
					add(c.GetConstraint())
				}
			}
		}
	}
	return stated, nil
}
