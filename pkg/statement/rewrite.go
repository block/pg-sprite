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

// Typed refusals for advisory rewrites. The rewriters flip one syntactic
// flag and deparse — no semantics are derived; a statement that cannot be
// rewritten that way is refused with one of these.
var (
	// ErrNotRewritable is returned when the statement kind has no
	// CONCURRENTLY form to rewrite to.
	ErrNotRewritable = errors.New("statement has no concurrent form")
	// ErrNotValidNotApplicable is returned when the statement is not a
	// single ADD CHECK / ADD FOREIGN KEY that could take NOT VALID.
	ErrNotValidNotApplicable = errors.New("statement cannot take NOT VALID")
)

// Concurrently returns sql rewritten to its CONCURRENTLY form: CREATE
// INDEX, DROP INDEX, REINDEX, or ALTER TABLE ... DETACH PARTITION. The
// rewrite flips the grammar's concurrency flag and deparses — nothing else
// changes. A statement already concurrent comes back canonicalized.
func Concurrently(sql string) (string, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return "", err
	}
	switch {
	case node.GetIndexStmt() != nil:
		node.GetIndexStmt().Concurrent = true
	case node.GetDropStmt() != nil && node.GetDropStmt().GetRemoveType() == pganalyze.ObjectType_OBJECT_INDEX:
		node.GetDropStmt().Concurrent = true
	case node.GetReindexStmt() != nil:
		re := node.GetReindexStmt()
		if !reindexConcurrent(re) {
			re.Params = append(re.Params, &pganalyze.Node{
				Node: &pganalyze.Node_DefElem{DefElem: &pganalyze.DefElem{Defname: "concurrently"}},
			})
		}
	case detachPartitionCmd(node) != nil:
		detachPartitionCmd(node).Concurrent = true
	default:
		return "", ErrNotRewritable
	}
	return deparseOne(node)
}

// AddNotValid rewrites a single-command ALTER TABLE ... ADD CONSTRAINT
// (CHECK or FOREIGN KEY) to its NOT VALID form and returns the rewritten
// statement plus the constraint name for the follow-up VALIDATE CONSTRAINT
// step.
//
// The VALIDATE step has to name the constraint, so an unnamed constraint is
// given the name PostgreSQL would choose first (defaultConstraintName) and
// the rewrite states it. The server moves past its first choice only when a
// constraint in the schema already holds it; stated explicitly, the name
// instead fails the NOT VALID step with a duplicate-object error when the
// table already has a constraint of that name, before anything is added.
func AddNotValid(sql string) (rewritten, constraint string, err error) {
	node, err := parseSingle(sql)
	if err != nil {
		return "", "", err
	}
	alter := node.GetAlterTableStmt()
	if alter == nil || alter.GetObjtype() != pganalyze.ObjectType_OBJECT_TABLE || len(alter.GetCmds()) != 1 {
		return "", "", ErrNotValidNotApplicable
	}
	cmd := alter.GetCmds()[0].GetAlterTableCmd()
	if cmd.GetSubtype() != pganalyze.AlterTableType_AT_AddConstraint {
		return "", "", ErrNotValidNotApplicable
	}
	con := cmd.GetDef().GetConstraint()
	validatable := con.GetContype() == pganalyze.ConstrType_CONSTR_CHECK ||
		con.GetContype() == pganalyze.ConstrType_CONSTR_FOREIGN
	if !validatable {
		return "", "", ErrNotValidNotApplicable
	}
	if con.GetConname() == "" {
		con.Conname = defaultConstraintName(alter.GetRelation().GetRelname(), con)
	}
	con.SkipValidation = true
	con.InitiallyValid = false
	if rewritten, err = deparseOne(node); err != nil {
		return "", "", err
	}
	return rewritten, con.GetConname(), nil
}

// defaultConstraintName mirrors the first choice of PostgreSQL's
// ChooseConstraintName for an unnamed CHECK or FOREIGN KEY added to table:
// `<table>_<columns>_fkey` over the referencing columns of a foreign key,
// and for a check `<table>_<column>_check` when its expression references
// exactly one distinct column, else `<table>_check`, truncated the way the
// server truncates (makeObjectName). The server counts the columns of the
// analyzed expression; this counts the column references of the parsed one,
// which agree except for a bare reference that resolves to the table's
// whole row, and the server itself calls the rule an approximation of the
// column-versus-table constraint distinction.
func defaultConstraintName(table string, con *pganalyze.Constraint) string {
	if con.GetContype() == pganalyze.ConstrType_CONSTR_FOREIGN {
		columns := make([]string, 0, len(con.GetFkAttrs()))
		for _, attr := range con.GetFkAttrs() {
			columns = append(columns, attr.GetString_().GetSval())
		}
		return makeObjectName(table, strings.Join(columns, "_"), "fkey")
	}
	var column string
	if refs := columnRefNames(con.GetRawExpr().ProtoReflect(), nil); len(refs) == 1 && refs[0] != wholeRowRef {
		column = refs[0]
	}
	return makeObjectName(table, column, "check")
}

// wholeRowRef stands for a `t.*` reference among column names. No column
// can be named the empty string, so a column quoted as "*" stays distinct.
const wholeRowRef = ""

// columnRefNames appends to names the distinct columns msg's parse tree
// references, each by its last name part (`t.qty` is `qty`) and a
// whole-row reference as wholeRowRef. The walk covers every message field, list,
// and map, so a reference inside a function call, CASE, or operand counts
// like one at the top.
func columnRefNames(msg protoreflect.Message, names []string) []string {
	if ref, ok := msg.Interface().(*pganalyze.ColumnRef); ok {
		fields := ref.GetFields()
		if len(fields) == 0 {
			return names
		}
		name := wholeRowRef
		if last := fields[len(fields)-1]; last.GetAStar() == nil {
			name = last.GetString_().GetSval()
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
		return names
	}
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		switch {
		case field.IsMap():
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
					names = columnRefNames(item.Message(), names)
					return true
				})
			}
		case field.IsList():
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				names = columnRefNames(list.Get(i).Message(), names)
			}
		default:
			names = columnRefNames(value.Message(), names)
		}
		return true
	})
	return names
}

// parseSingle parses sql and requires exactly one statement, returning its
// root node for in-place rewriting.
func parseSingle(sql string) (*pganalyze.Node, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse statement: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return nil, fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	return tree.GetStmts()[0].GetStmt(), nil
}

// detachPartitionCmd returns the PartitionCmd of a single-command
// ALTER TABLE ... DETACH PARTITION, or nil when the statement is anything
// else.
func detachPartitionCmd(node *pganalyze.Node) *pganalyze.PartitionCmd {
	alter := node.GetAlterTableStmt()
	if alter == nil || len(alter.GetCmds()) != 1 {
		return nil
	}
	cmd := alter.GetCmds()[0].GetAlterTableCmd()
	if cmd.GetSubtype() != pganalyze.AlterTableType_AT_DetachPartition {
		return nil
	}
	return cmd.GetDef().GetPartitionCmd()
}
