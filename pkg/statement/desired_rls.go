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

// ErrPolicyRelationDependency refuses a policy expression that reads a table it
// does not name schema-qualified. Policy expressions resolve against nothing but
// the scratch schema and PostgreSQL's built-ins, so an unqualified name either
// fails to resolve or binds an object nobody meant. A CTE name is refused the
// same way, because the grammar cannot tell it from a table name.
var ErrPolicyRelationDependency = errors.New("a policy must name every table it reads schema-qualified, such as public.team_members")

// ErrPolicyReadsOwnTable refuses a policy expression that reads a relation with
// its own table's name. The scratch build would bind the name to the live table
// rather than to the desired one, and PostgreSQL reports infinite recursion for
// a policy that reads the table it protects. The file does not say which schema
// the table lives in, so the name is refused in every schema.
var ErrPolicyReadsOwnTable = errors.New("a policy cannot read the table it is defined on")

// ErrPolicyReadsCatalog refuses a policy expression that reads a pg_catalog
// table. pg_catalog is always searched, so introspection prints such a table
// unqualified, and the file pull would export from the live policy could not
// be admitted again.
var ErrPolicyReadsCatalog = errors.New("a policy cannot read a pg_catalog table")

// ErrPolicyLocksRows refuses a policy expression whose query locks the rows it
// reads (FOR UPDATE, FOR SHARE, and their weaker forms). PostgreSQL takes ROW
// SHARE on such a table while it builds the policy, which waits behind a
// session holding EXCLUSIVE, where a plain read takes only ACCESS SHARE.
var ErrPolicyLocksRows = errors.New("a policy cannot lock the rows it reads, such as with FOR SHARE or FOR UPDATE")

// ErrRowSecurityForeignKey refuses a foreign key in a row-security desired
// file. The row-security scratch build creates no stand-in for a referenced
// table, so a foreign key there would bind to the live table and lock it.
var ErrRowSecurityForeignKey = errors.New("foreign keys are not supported in desired row security")

// DesiredWithRowSecurity proves declaration syntax for scratch inspection and
// the dedicated atomic RLS executor. It is distinct from DesiredSchema; generic
// native and create executors cannot accept it. Live safety needs executor checks.
// The explicit parse entry point owns the complete table-local RLS definition,
// including an empty policy set. Only ParseDesiredWithRowSecurity constructs it.
type DesiredWithRowSecurity struct {
	table       string
	definition  DesiredSchema
	statements  []Statement
	policyReads []QualifiedName
}

// Table returns the declaration's unqualified table name.
func (d DesiredWithRowSecurity) Table() string { return d.table }

// TableDefinition returns the CREATE TABLE and its indexes without the row
// security that follows them. Running it on its own creates a table without
// its row security, so only the executor that creates the table and its row
// security in one transaction consumes it.
func (d DesiredWithRowSecurity) TableDefinition() DesiredSchema { return d.definition }

// Statements returns a defensive copy in scratch execution order.
func (d DesiredWithRowSecurity) Statements() []Statement { return slices.Clone(d.statements) }

// ExtensionSchemas returns the schemas the table definition's unqualified
// names also resolve against, as [DesiredSchema.ExtensionSchemas] does. It
// is empty unless [DesiredWithRowSecurity.WithExtensionSchemas] set it. The
// slice is a copy.
func (d DesiredWithRowSecurity) ExtensionSchemas() []string { return d.definition.ExtensionSchemas() }

// WithExtensionSchemas returns a copy of d whose table definition resolves
// unqualified type, function, and operator names against schemas as well,
// under [DesiredSchema.WithExtensionSchemas]'s rules. Only the CREATE TABLE
// and its indexes widen. Policy expressions still resolve nothing but the
// table and PostgreSQL's built-ins, so every helper and table a policy
// names stays qualified, and an extension schema can never change which
// function a policy calls.
func (d DesiredWithRowSecurity) WithExtensionSchemas(schemas ...string) (DesiredWithRowSecurity, error) {
	definition, err := d.definition.WithExtensionSchemas(schemas...)
	if err != nil {
		return DesiredWithRowSecurity{}, err
	}
	d.definition = definition
	return d, nil
}

// PolicyReads returns the tables the policies' expressions read, each
// schema-qualified as the file names it, sorted and without duplicates. The
// slice is a copy.
func (d DesiredWithRowSecurity) PolicyReads() []QualifiedName { return slices.Clone(d.policyReads) }

// ParseDesiredWithRowSecurity admits table/index definitions followed by RLS
// settings, policies, and policy comments. It requires explicit ENABLE/DISABLE;
// FORCE/NO FORCE is optional and cannot be declared twice. Other ALTER commands,
// qualified targets, and policy queries reading a table unqualified or reading
// their own table are refused.
func ParseDesiredWithRowSecurity(sql string) (DesiredWithRowSecurity, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return DesiredWithRowSecurity{}, fmt.Errorf("%w: %w", ErrParseDesired, err)
	}
	return parseDesiredWithRowSecurityTree(tree)
}

// parseDesiredWithRowSecurityTree applies ParseDesiredWithRowSecurity's
// admission rules to a parsed file.
func parseDesiredWithRowSecurityTree(tree *pganalyze.ParseResult) (DesiredWithRowSecurity, error) {
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
	if table.HasForeignKeys() {
		return DesiredWithRowSecurity{}, fmt.Errorf("%w: table %q", ErrRowSecurityForeignKey, table.Table())
	}
	d := DesiredWithRowSecurity{table: table.Table(), definition: table, statements: table.Statements()}
	enabled, forced := 0, 0
	var comments []Statement
	for _, node := range security {
		target, reads, err := admitRowSecurity(node, &enabled, &forced)
		if err != nil {
			return DesiredWithRowSecurity{}, err
		}
		d.policyReads = append(d.policyReads, reads...)
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
	slices.SortFunc(d.policyReads, compareQualifiedNames)
	d.policyReads = slices.Compact(d.policyReads)
	return d, nil
}

// admitRowSecurity admits one row-security statement and returns the table it
// targets and, for a policy, the tables its expressions read.
func admitRowSecurity(node *pganalyze.Node, enabled, forced *int) (string, []QualifiedName, error) {
	switch {
	case node.GetAlterTableStmt() != nil:
		alter := node.GetAlterTableStmt()
		if alter.GetMissingOk() || alter.GetObjtype() != pganalyze.ObjectType_OBJECT_TABLE {
			return "", nil, ErrDisallowedStatement
		}
		for _, cmd := range alter.GetCmds() {
			switch cmd.GetAlterTableCmd().GetSubtype() {
			case pganalyze.AlterTableType_AT_EnableRowSecurity, pganalyze.AlterTableType_AT_DisableRowSecurity:
				*enabled += 1
			case pganalyze.AlterTableType_AT_ForceRowSecurity, pganalyze.AlterTableType_AT_NoForceRowSecurity:
				*forced += 1
			default:
				return "", nil, ErrDisallowedStatement
			}
		}
		target, err := rowSecurityTarget(alter.GetRelation())
		return target, nil, err
	case node.GetCreatePolicyStmt() != nil:
		policy := node.GetCreatePolicyStmt()
		target, err := rowSecurityTarget(policy.GetTable())
		if err != nil {
			return "", nil, err
		}
		reads, err := admitPolicyRelations(policy, target)
		if err != nil {
			return "", nil, err
		}
		return target, reads, nil
	case node.GetCommentStmt() != nil:
		comment := node.GetCommentStmt()
		if comment.GetObjtype() != pganalyze.ObjectType_OBJECT_POLICY {
			return "", nil, ErrDisallowedStatement
		}
		names := comment.GetObject().GetList().GetItems()
		if len(names) != 2 {
			return "", nil, ErrQualifiedName
		}
		return names[0].GetString_().GetSval(), nil, nil
	default:
		return "", nil, ErrDisallowedStatement
	}
}

func rowSecurityTarget(rel *pganalyze.RangeVar) (string, error) {
	if rel.GetSchemaname() != "" || rel.GetCatalogname() != "" {
		return "", ErrQualifiedName
	}
	return rel.GetRelname(), nil
}

// admitPolicyRelations admits the tables a policy's USING and WITH CHECK
// expressions read and returns them: each must be named schema-qualified, outside pg_catalog,
// and none may carry the name of the table the policy is defined on, and no
// query may lock the rows it reads (see ErrPolicyRelationDependency,
// ErrPolicyReadsCatalog, ErrPolicyReadsOwnTable, and ErrPolicyLocksRows). This is grammar admission only: a scalar SELECT
// auth.uid() reads no table and is admitted unchanged. SQL meaning still comes
// exclusively from execute-and-introspect, where a qualified name binds the live
// table the applied policy will read.
func admitPolicyRelations(policy *pganalyze.CreatePolicyStmt, table string) ([]QualifiedName, error) {
	var read []*pganalyze.RangeVar
	for _, expr := range []*pganalyze.Node{policy.GetQual(), policy.GetWithCheck()} {
		if expr == nil {
			continue
		}
		if locksRows(expr.ProtoReflect()) {
			return nil, fmt.Errorf("%w: policy %q locks the rows it reads", ErrPolicyLocksRows, policy.GetPolicyName())
		}
		read = appendRelations(read, expr.ProtoReflect())
	}
	reads := make([]QualifiedName, 0, len(read))
	for _, rel := range read {
		if rel.GetSchemaname() == "" || rel.GetCatalogname() != "" {
			return nil, fmt.Errorf("%w: policy %q reads %q", ErrPolicyRelationDependency, policy.GetPolicyName(), relationName(rel))
		}
		if rel.GetSchemaname() == "pg_catalog" {
			return nil, fmt.Errorf("%w: policy %q reads %q", ErrPolicyReadsCatalog, policy.GetPolicyName(), relationName(rel))
		}
		if rel.GetRelname() == table {
			return nil, fmt.Errorf("%w: policy %q reads %q", ErrPolicyReadsOwnTable, policy.GetPolicyName(), relationName(rel))
		}
		reads = append(reads, QualifiedName{Schema: rel.GetSchemaname(), Name: rel.GetRelname()})
	}
	return reads, nil
}

// relationName prints a RangeVar the way the file wrote it, for an error.
func relationName(rel *pganalyze.RangeVar) string {
	parts := []string{rel.GetCatalogname(), rel.GetSchemaname(), rel.GetRelname()}
	return strings.Join(slices.DeleteFunc(parts, func(part string) bool { return part == "" }), ".")
}

// appendRelations appends every RangeVar in msg, in tree order.
func appendRelations(read []*pganalyze.RangeVar, msg protoreflect.Message) []*pganalyze.RangeVar {
	walkMessages(msg, func(node protoreflect.Message) bool {
		if rel, ok := node.Interface().(*pganalyze.RangeVar); ok {
			read = append(read, rel)
		}
		return true
	})
	return read
}

// locksRows reports whether any query in msg carries a locking clause.
func locksRows(msg protoreflect.Message) bool {
	found := false
	walkMessages(msg, func(node protoreflect.Message) bool {
		_, found = node.Interface().(*pganalyze.LockingClause)
		return !found
	})
	return found
}

// HasRowSecurityDeclaration reports whether sql declares row security without
// admitting it. Use it to ask that question alone, for example to skip input
// that declares no row security, or to inspect a single statement rather than
// a desired file. To route a desired file to its planner, use
// [ParseDesiredFile], which makes this check and applies the matching front
// door in one parse. The result is not an admission: execution still needs a
// value from ParseDesiredWithRowSecurity or ParseDesiredFile.
func HasRowSecurityDeclaration(sql string) (bool, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrParseDesired, err)
	}
	return hasRowSecurity(tree), nil
}

// hasRowSecurity reports whether a parsed file declares row security: a
// policy, a policy comment, or a row-security setting on a table.
func hasRowSecurity(tree *pganalyze.ParseResult) bool {
	for _, raw := range tree.GetStmts() {
		node := raw.GetStmt()
		if node.GetCreatePolicyStmt() != nil {
			return true
		}
		if c := node.GetCommentStmt(); c != nil {
			if c.GetObjtype() == pganalyze.ObjectType_OBJECT_POLICY {
				return true
			}
		}
		if a := node.GetAlterTableStmt(); a != nil {
			for _, cmd := range a.GetCmds() {
				switch cmd.GetAlterTableCmd().GetSubtype() {
				case pganalyze.AlterTableType_AT_EnableRowSecurity, pganalyze.AlterTableType_AT_DisableRowSecurity,
					pganalyze.AlterTableType_AT_ForceRowSecurity, pganalyze.AlterTableType_AT_NoForceRowSecurity:
					return true
				}
			}
		}
	}
	return false
}
