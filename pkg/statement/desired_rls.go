package statement

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ErrRowSecurityDeclaration reports an incomplete or contradictory RLS declaration.
var ErrRowSecurityDeclaration = errors.New("desired row security requires one explicit ENABLE or DISABLE on the desired table")

// ErrRowSecurityTableDefinitionRequired identifies a missing table definition in an RLS file.
var ErrRowSecurityTableDefinitionRequired = errors.New("desired row security requires a CREATE TABLE definition")

// ErrPolicyRelationDependency refuses policy subqueries that read relations until
// desired-state materialization can preserve their dependency identities.
var ErrPolicyRelationDependency = errors.New("policy relation dependencies are not supported in desired row security")

// DesiredWithRowSecurity proves declaration syntax for scratch inspection and
// the dedicated atomic RLS executor. It is distinct from DesiredSchema; generic
// native and create executors cannot accept it. Live safety needs executor checks.
// The explicit parse entry point owns the complete table-local RLS definition,
// including an empty policy set. Only ParseDesiredWithRowSecurity constructs it.
type DesiredWithRowSecurity struct {
	table      string
	statements []Statement
}

// Table returns the declaration's unqualified table name.
func (d DesiredWithRowSecurity) Table() string { return d.table }

// Statements returns a defensive copy in scratch execution order.
func (d DesiredWithRowSecurity) Statements() []Statement { return slices.Clone(d.statements) }

// ParseDesiredWithRowSecurity admits table/index definitions followed by RLS
// settings, policies, and policy comments. It requires explicit ENABLE/DISABLE;
// FORCE/NO FORCE is optional and cannot be declared twice. Other ALTER commands,
// qualified targets, and policy queries reading relations are refused.
func ParseDesiredWithRowSecurity(sql string) (DesiredWithRowSecurity, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return DesiredWithRowSecurity{}, fmt.Errorf("parse desired row security: %w", err)
	}
	var tableSQL strings.Builder
	var security []*pganalyze.Node
	hasTable := false
	for _, raw := range tree.GetStmts() {
		node := raw.GetStmt()
		if node.GetCreateStmt() != nil {
			hasTable = true
		}
		if node.GetCreateStmt() != nil || node.GetIndexStmt() != nil {
			text, err := deparseOne(node)
			if err != nil {
				return DesiredWithRowSecurity{}, err
			}
			tableSQL.WriteString(text + ";\n")
		} else {
			security = append(security, node)
		}
	}
	if !hasTable {
		return DesiredWithRowSecurity{}, fmt.Errorf("%w: %w", ErrRowSecurityTableDefinitionRequired, ErrRowSecurityDeclaration)
	}
	table, err := ParseDesired(tableSQL.String())
	if err != nil {
		return DesiredWithRowSecurity{}, err
	}
	d := DesiredWithRowSecurity{table: table.Table(), statements: table.Statements()}
	enabled, forced := 0, 0
	var comments []Statement
	for _, node := range security {
		target, err := admitRowSecurity(node, &enabled, &forced)
		if err != nil {
			return DesiredWithRowSecurity{}, err
		}
		if target != d.table {
			return DesiredWithRowSecurity{}, ErrRowSecurityDeclaration
		}
		text, err := deparseOne(node)
		if err != nil {
			return DesiredWithRowSecurity{}, err
		}
		st := Statement{sql: text, kind: KindProvisioning, table: target}
		if node.GetCommentStmt() != nil {
			comments = append(comments, st)
		} else {
			d.statements = append(d.statements, st)
		}
	}
	if enabled != 1 || forced > 1 {
		return DesiredWithRowSecurity{}, ErrRowSecurityDeclaration
	}
	d.statements = append(d.statements, comments...)
	return d, nil
}

func admitRowSecurity(node *pganalyze.Node, enabled, forced *int) (string, error) {
	switch {
	case node.GetAlterTableStmt() != nil:
		alter := node.GetAlterTableStmt()
		if alter.GetMissingOk() || alter.GetObjtype() != pganalyze.ObjectType_OBJECT_TABLE {
			return "", ErrDisallowedStatement
		}
		for _, cmd := range alter.GetCmds() {
			switch cmd.GetAlterTableCmd().GetSubtype() {
			case pganalyze.AlterTableType_AT_EnableRowSecurity, pganalyze.AlterTableType_AT_DisableRowSecurity:
				*enabled += 1
			case pganalyze.AlterTableType_AT_ForceRowSecurity, pganalyze.AlterTableType_AT_NoForceRowSecurity:
				*forced += 1
			default:
				return "", ErrDisallowedStatement
			}
		}
		return rowSecurityTarget(alter.GetRelation())
	case node.GetCreatePolicyStmt() != nil:
		policy := node.GetCreatePolicyStmt()
		if policyReadsRelation(policy.GetQual()) || policyReadsRelation(policy.GetWithCheck()) {
			return "", ErrPolicyRelationDependency
		}
		return rowSecurityTarget(policy.GetTable())
	case node.GetCommentStmt() != nil:
		comment := node.GetCommentStmt()
		if comment.GetObjtype() != pganalyze.ObjectType_OBJECT_POLICY {
			return "", ErrDisallowedStatement
		}
		names := comment.GetObject().GetList().GetItems()
		if len(names) != 2 {
			return "", ErrQualifiedName
		}
		return names[0].GetString_().GetSval(), nil
	default:
		return "", ErrDisallowedStatement
	}
}

func rowSecurityTarget(rel *pganalyze.RangeVar) (string, error) {
	if rel.GetSchemaname() != "" || rel.GetCatalogname() != "" {
		return "", ErrQualifiedName
	}
	return rel.GetRelname(), nil
}

// This is grammar admission only: reject RangeVar nodes in expressions. A
// scalar SELECT auth.uid() has none and remains admissible; SELECT FROM does
// not. SQL meaning still comes exclusively from execute-and-introspect.
func policyReadsRelation(node *pganalyze.Node) bool {
	if node == nil {
		return false
	}
	return messageReadsRelation(node.ProtoReflect())
}

func messageReadsRelation(msg protoreflect.Message) bool {
	if _, ok := msg.Interface().(*pganalyze.RangeVar); ok {
		return true
	}
	found := false
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		switch {
		case field.IsMap():
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
					found = messageReadsRelation(item.Message())
					return !found
				})
			}
		case field.IsList():
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if messageReadsRelation(list.Get(i).Message()) {
					found = true
					break
				}
			}
		default:
			found = messageReadsRelation(value.Message())
		}
		return !found
	})
	return found
}

// HasRowSecurityDeclaration detects RLS syntax without admitting the file for
// execution. Callers must still use ParseDesiredWithRowSecurity for validation.
func HasRowSecurityDeclaration(sql string) (bool, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return false, fmt.Errorf("parse desired schema: %w", err)
	}
	for _, raw := range tree.GetStmts() {
		node := raw.GetStmt()
		if node.GetCreatePolicyStmt() != nil {
			return true, nil
		}
		if c := node.GetCommentStmt(); c != nil {
			if c.GetObjtype() == pganalyze.ObjectType_OBJECT_POLICY {
				return true, nil
			}
		}
		if a := node.GetAlterTableStmt(); a != nil {
			for _, cmd := range a.GetCmds() {
				switch cmd.GetAlterTableCmd().GetSubtype() {
				case pganalyze.AlterTableType_AT_EnableRowSecurity, pganalyze.AlterTableType_AT_DisableRowSecurity,
					pganalyze.AlterTableType_AT_ForceRowSecurity, pganalyze.AlterTableType_AT_NoForceRowSecurity:
					return true, nil
				}
			}
		}
	}
	return false, nil
}
