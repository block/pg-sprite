package statement

import (
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// SecurityStatements returns only the declaration's admitted RLS statements,
// qualified for the requested schema. Table/index definitions are never replayed.
// This does not prove live execution is safe: the dedicated executor must lock,
// compare the table definitions, enforce budgets, and verify the resulting state.
func (d DesiredWithRowSecurity) SecurityStatements(schema string) ([]string, error) {
	if d.table == "" || schema == "" {
		return nil, ErrRowSecurityDeclaration
	}
	var result []string
	for _, st := range d.statements {
		if st.kind != KindProvisioning {
			continue
		}
		tree, err := pgquery.Parse(st.sql)
		if err != nil {
			return nil, fmt.Errorf("parse row security statement: %w", err)
		}
		if len(tree.GetStmts()) != 1 {
			return nil, ErrNotOneStatement
		}
		node := tree.GetStmts()[0].GetStmt()
		enabled, forced := 0, 0
		target, err := admitRowSecurity(node, &enabled, &forced)
		if err != nil {
			return nil, err
		}
		if target != d.table {
			return nil, ErrRowSecurityDeclaration
		}
		switch {
		case node.GetAlterTableStmt() != nil:
			node.GetAlterTableStmt().Relation.Schemaname = schema
		case node.GetCreatePolicyStmt() != nil:
			node.GetCreatePolicyStmt().Table.Schemaname = schema
		case node.GetCommentStmt() != nil:
			list := node.GetCommentStmt().Object.GetList()
			name := &pganalyze.Node{Node: &pganalyze.Node_String_{String_: &pganalyze.String{Sval: schema}}}
			list.Items = append([]*pganalyze.Node{name}, list.Items...)
		default:
			return nil, ErrDisallowedStatement
		}
		sql, err := deparseOne(node)
		if err != nil {
			return nil, err
		}
		result = append(result, sql)
	}
	return result, nil
}
