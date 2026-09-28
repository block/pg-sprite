package statement

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	pgproto "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// ErrRowSecurityChange identifies SQL outside a table-local RLS operation.
var ErrRowSecurityChange = errors.New("invalid row security operation")

// RowSecurityChange is an ordered SQL operation for comparison, not execution
// authority. Parsing checks statement kinds and one fully qualified target; it
// does not check privileges, policy dependencies, or whether access widens.
// It deliberately stays separate from Statement and ordinary native DDL.
// The PostgreSQL executor must still admit and execute the operation atomically.
type RowSecurityChange struct {
	schema     string
	table      string
	statements []string
	canonical  []string
}

// Schema returns the physical schema named by every target statement.
func (c RowSecurityChange) Schema() string { return c.schema }

// Table returns the table named by every target statement.
func (c RowSecurityChange) Table() string { return c.table }

// Statements returns a copy of the original statements in their original order.
func (c RowSecurityChange) Statements() []string { return slices.Clone(c.statements) }

// CanonicalSQL preserves order and duplicates for a single operation's comparison
// key. It preserves physical schema qualifiers, roles, predicates, and comments;
// namespace mapping is a separate concern. A zero value is not a parsed operation.
func (c RowSecurityChange) CanonicalSQL() string { return strings.Join(c.canonical, ";\n") }

// ParseRowSecurityChange admits the SQL shapes emitted by a complete atomic RLS
// replacement: policy drops/creates/comments and RLS settings on one table.
// The complete script must contain only these statements. Empty input is not a change.
// This parser does not admit statements to the native executor.
func ParseRowSecurityChange(sql string) (RowSecurityChange, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return RowSecurityChange{}, fmt.Errorf("%w: %w", ErrRowSecurityChange, err)
	}
	if len(tree.GetStmts()) == 0 {
		return RowSecurityChange{}, ErrRowSecurityChange
	}
	schema, table := rowSecurityOperationTarget(tree.Stmts[0].Stmt)
	if schema == "" || table == "" {
		return RowSecurityChange{}, ErrRowSecurityChange
	}
	result := RowSecurityChange{schema: schema, table: table}
	for i, raw := range tree.Stmts {
		if !rowSecurityStatementTarget(raw.Stmt, schema, table) {
			return RowSecurityChange{}, fmt.Errorf("%w: statement %d has an unsupported shape or target", ErrRowSecurityChange, i+1)
		}
		canonical, err := deparseOne(raw.Stmt)
		if err != nil {
			return RowSecurityChange{}, fmt.Errorf("%w: %w", ErrRowSecurityChange, err)
		}
		result.canonical = append(result.canonical, canonical)
	}
	source, err := Split(sql)
	if err != nil {
		return RowSecurityChange{}, fmt.Errorf("%w: %w", ErrRowSecurityChange, err)
	}
	for _, stmt := range source {
		result.statements = append(result.statements, stmt.SQL)
	}
	return result, nil
}

func rowSecurityOperationTarget(node *pgproto.Node) (string, string) {
	var relation *pgproto.RangeVar
	var object *pgproto.Node
	switch {
	case node.GetCreatePolicyStmt() != nil:
		relation = node.GetCreatePolicyStmt().GetTable()
	case node.GetAlterTableStmt() != nil:
		relation = node.GetAlterTableStmt().GetRelation()
	case node.GetDropStmt() != nil:
		if len(node.GetDropStmt().GetObjects()) == 1 {
			object = node.GetDropStmt().Objects[0]
		}
	case node.GetCommentStmt() != nil:
		object = node.GetCommentStmt().GetObject()
	}
	if relation != nil {
		return relation.GetSchemaname(), relation.GetRelname()
	}
	names := object.GetList().GetItems()
	if len(names) != 3 {
		return "", ""
	}
	return names[0].GetString_().GetSval(), names[1].GetString_().GetSval()
}

func rowSecurityStatementTarget(node *pgproto.Node, schema, table string) bool {
	switch {
	case node.GetCreatePolicyStmt() != nil:
		return rowSecurityRelationMatches(node.GetCreatePolicyStmt().GetTable(), schema, table)
	case node.GetDropStmt() != nil:
		drop := node.GetDropStmt()
		if drop.GetRemoveType() != pgproto.ObjectType_OBJECT_POLICY || drop.GetMissingOk() {
			return false
		}
		if drop.GetBehavior() != pgproto.DropBehavior_DROP_RESTRICT || len(drop.GetObjects()) != 1 {
			return false
		}
		return rowSecurityPolicyMatches(drop.Objects[0], schema, table)
	case node.GetCommentStmt() != nil:
		comment := node.GetCommentStmt()
		return comment.GetObjtype() == pgproto.ObjectType_OBJECT_POLICY && rowSecurityPolicyMatches(comment.GetObject(), schema, table)
	case node.GetAlterTableStmt() != nil:
		alter := node.GetAlterTableStmt()
		if !rowSecurityRelationMatches(alter.GetRelation(), schema, table) {
			return false
		}
		if alter.GetObjtype() != pgproto.ObjectType_OBJECT_TABLE || alter.GetMissingOk() || len(alter.GetCmds()) != 1 {
			return false
		}
		switch alter.Cmds[0].GetAlterTableCmd().GetSubtype() {
		case pgproto.AlterTableType_AT_EnableRowSecurity, pgproto.AlterTableType_AT_DisableRowSecurity,
			pgproto.AlterTableType_AT_ForceRowSecurity, pgproto.AlterTableType_AT_NoForceRowSecurity:
			return true
		}
	}
	return false
}

func rowSecurityRelationMatches(rel *pgproto.RangeVar, schema, table string) bool {
	if rel.GetCatalogname() != "" {
		return false
	}
	return rel.GetSchemaname() == schema && rel.GetRelname() == table
}

func rowSecurityPolicyMatches(node *pgproto.Node, schema, table string) bool {
	names := node.GetList().GetItems()
	if len(names) != 3 {
		return false
	}
	if names[0].GetString_().GetSval() != schema || names[1].GetString_().GetSval() != table {
		return false
	}
	return names[2].GetString_().GetSval() != ""
}

// CanonicalSQLForNamespace omits only the operation's target schema qualifier.
// Use it when the comparison key already contains a canonical namespace and
// table. Qualified policy helpers and all expressions remain unchanged.
// This is a comparison representation, never executable SQL.
func (c RowSecurityChange) CanonicalSQLForNamespace() (string, error) {
	if c.schema == "" || c.table == "" {
		return "", ErrRowSecurityChange
	}
	parts := make([]string, 0, len(c.canonical))
	for _, sql := range c.canonical {
		node, err := parseSingle(sql)
		if err != nil {
			return "", err
		}
		switch {
		case node.GetCreatePolicyStmt() != nil:
			node.GetCreatePolicyStmt().Table.Schemaname = ""
		case node.GetAlterTableStmt() != nil:
			node.GetAlterTableStmt().Relation.Schemaname = ""
		case node.GetDropStmt() != nil:
			object := node.GetDropStmt().Objects[0].GetList()
			object.Items = object.Items[1:]
		case node.GetCommentStmt() != nil:
			object := node.GetCommentStmt().Object.GetList()
			object.Items = object.Items[1:]
		default:
			return "", ErrRowSecurityChange
		}
		canonical, err := deparseOne(node)
		if err != nil {
			return "", err
		}
		parts = append(parts, canonical)
	}
	return strings.Join(parts, ";\n"), nil
}
