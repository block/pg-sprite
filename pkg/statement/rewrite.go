package statement

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
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
		if !reindexConcurrently(re) {
			// A CONCURRENTLY false is replaced, not joined: the server
			// refuses a repeated option.
			re.Params = slices.DeleteFunc(re.Params, isConcurrentlyOption)
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

// ConstraintNaming is what the server consults when it names an unnamed
// constraint, read from the live table. The zero value knows nothing: a
// bare name reads as a column and no name counts as taken, which yields the
// server's first choice.
type ConstraintNaming struct {
	// Columns are the table's live columns. A bare name in a CHECK reads
	// as a column when the table has one by that name and otherwise, when
	// it is the table's own name, as the table's whole row. Nil reads every
	// bare name as a column.
	Columns map[string]bool
	// Taken are the names of the constraints already in the table's
	// schema, every table's and domain's alike: the server passes over each
	// of them when it picks a name.
	Taken map[string]bool
}

// AddNotValid rewrites a single-command ALTER TABLE ... ADD CONSTRAINT
// (CHECK or FOREIGN KEY) to its NOT VALID form and returns the rewritten
// statement plus the constraint name for the follow-up VALIDATE CONSTRAINT
// step.
//
// The VALIDATE step has to name the constraint, so an unnamed constraint is
// given the name PostgreSQL would choose (defaultConstraintName) and the
// rewrite states it. The server takes its first choice unless a constraint
// in the schema already holds it, and then tries the label with 1, 2, ...
// appended; naming.Taken lets the rewrite make the same moves. A name the
// rewrite states that the table already uses fails the NOT VALID step with
// a duplicate-object error, before anything is added.
func AddNotValid(sql string, naming ConstraintNaming) (rewritten, constraint string, err error) {
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
		con.Conname = defaultConstraintName(alter.GetRelation().GetRelname(), con, naming)
	}
	con.SkipValidation = true
	con.InitiallyValid = false
	if rewritten, err = deparseOne(node); err != nil {
		return "", "", err
	}
	return rewritten, con.GetConname(), nil
}

// defaultConstraintName mirrors PostgreSQL's ChooseConstraintName for an
// unnamed CHECK or FOREIGN KEY added to table: `<table>_<columns>_fkey`
// over the referencing columns of a foreign key, and for a check
// `<table>_<column>_check` when its expression references exactly one
// distinct column, else `<table>_check`, truncated the way the server
// truncates (makeObjectName). A name naming.Taken holds moves on to the
// label with 1, 2, ... appended, as the server's does.
//
// The server counts the columns of the analyzed expression; this counts
// the column references of the parsed one, resolved the way the server
// resolves them against the one table in scope (checkColumn). The server
// itself calls the rule an approximation of the column-versus-table
// constraint distinction.
func defaultConstraintName(table string, con *pganalyze.Constraint, naming ConstraintNaming) string {
	var column, label string
	if con.GetContype() == pganalyze.ConstrType_CONSTR_FOREIGN {
		columns := make([]string, 0, len(con.GetFkAttrs()))
		for _, attr := range con.GetFkAttrs() {
			columns = append(columns, attr.GetString_().GetSval())
		}
		column, label = strings.Join(columns, "_"), "fkey"
	} else {
		refs := checkColumns(con.GetRawExpr().ProtoReflect(), table, naming.Columns, nil)
		if len(refs) == 1 && refs[0] != wholeRowRef {
			column = refs[0]
		}
		label = "check"
	}
	name := makeObjectName(table, column, label)
	for pass := 1; naming.Taken[name]; pass++ {
		name = makeObjectName(table, column, label+strconv.Itoa(pass))
	}
	return name
}

// wholeRowRef stands for a whole-row reference among column names. No
// column can be named the empty string, so a column quoted as "*" stays
// distinct.
const wholeRowRef = ""

// checkColumns appends to names the distinct columns of table that msg's
// parse tree references, resolving each reference with checkColumn. The
// walk covers every message field, list, and map, so a reference inside a
// function call, CASE, or operand counts like one at the top.
//
// Two forms read columns other than the reference they wrap, as the
// server's analysis resolves them. A field selected from the whole row,
// `(t).qty` or `(t.*).qty`, reads that column alone. A whole-row star
// inside ROW(...), `ROW(t.*)`, expands to every column of the table, which
// takes columns to be known; without them it counts as the whole row.
func checkColumns(msg protoreflect.Message, table string, columns map[string]bool, names []string) []string {
	addName := func(name string) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	switch node := msg.Interface().(type) {
	case *pganalyze.ColumnRef:
		if name, ok := checkColumn(node.GetFields(), table, columns); ok {
			addName(name)
		}
		return names
	case *pganalyze.A_Indirection:
		if field, ok := wholeRowField(node, table, columns); ok {
			addName(field)
			for _, ind := range node.GetIndirection()[1:] {
				names = checkColumns(ind.ProtoReflect(), table, columns, names)
			}
			return names
		}
	case *pganalyze.RowExpr:
		if columns != nil {
			for _, arg := range node.GetArgs() {
				if ref := arg.GetColumnRef(); ref != nil && isWholeRowStar(ref, table) {
					for column := range columns {
						addName(column)
					}
					continue
				}
				names = checkColumns(arg.ProtoReflect(), table, columns, names)
			}
			return names
		}
	}
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		switch {
		case field.IsMap():
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
					names = checkColumns(item.Message(), table, columns, names)
					return true
				})
			}
		case field.IsList():
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				names = checkColumns(list.Get(i).Message(), table, columns, names)
			}
		default:
			names = checkColumns(value.Message(), table, columns, names)
		}
		return true
	})
	return names
}

// checkColumn resolves a column reference in a CHECK on table to the column
// it reads, or wholeRowRef, the way the server's transformColumnRef does
// with the table as the only relation in scope. A qualified reference
// (`t.qty`, `s.t.qty`, `c.s.t.qty`) names the column last, or the whole row
// with a star; a field of a composite column is written `(addr).zip`, which
// wraps a reference to addr rather than extending it. A bare name is a
// column unless columns, when known, has no such column and the name is
// the table's, which makes it the whole row.
func checkColumn(fields []*pganalyze.Node, table string, columns map[string]bool) (string, bool) {
	if len(fields) == 0 {
		return "", false
	}
	last := fields[len(fields)-1]
	if last.GetAStar() != nil {
		return wholeRowRef, true
	}
	name := last.GetString_().GetSval()
	if len(fields) == 1 && name == table && columns != nil && !columns[name] {
		return wholeRowRef, true
	}
	return name, true
}

// wholeRowField reports the column a field selection from the table's
// whole row names: `(t).qty` and `(t.*).qty` read qty.
func wholeRowField(ind *pganalyze.A_Indirection, table string, columns map[string]bool) (string, bool) {
	ref := ind.GetArg().GetColumnRef()
	if ref == nil || len(ind.GetIndirection()) == 0 {
		return "", false
	}
	field := ind.GetIndirection()[0].GetString_()
	if field == nil {
		return "", false
	}
	if name, ok := checkColumn(ref.GetFields(), table, columns); !ok || name != wholeRowRef {
		return "", false
	}
	return field.GetSval(), true
}

// isWholeRowStar reports `t.*` or a bare `*`, the forms ROW(...) expands
// to the table's columns.
func isWholeRowStar(ref *pganalyze.ColumnRef, table string) bool {
	fields := ref.GetFields()
	if len(fields) == 0 || fields[len(fields)-1].GetAStar() == nil {
		return false
	}
	return len(fields) == 1 || fields[len(fields)-2].GetString_().GetSval() == table
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
