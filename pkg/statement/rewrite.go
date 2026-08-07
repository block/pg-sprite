package statement

import (
	"errors"
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// Typed refusals for advisory rewrites. The rewriters flip one syntactic
// flag and deparse — no semantics are derived; a statement that cannot be
// rewritten that way is refused with one of these.
var (
	// ErrNotRewritable is returned when the statement kind has no
	// CONCURRENTLY form to rewrite to.
	ErrNotRewritable = errors.New("statement has no concurrent form")
	// ErrNotValidNotApplicable is returned when the statement is not a
	// single named ADD CHECK / ADD FOREIGN KEY that could take NOT VALID.
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
// (named CHECK or FOREIGN KEY) to its NOT VALID form and returns the
// rewritten statement plus the constraint name for the follow-up
// VALIDATE CONSTRAINT step.
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
	if !validatable || con.GetConname() == "" {
		return "", "", ErrNotValidNotApplicable
	}
	con.SkipValidation = true
	con.InitiallyValid = false
	if rewritten, err = deparseOne(node); err != nil {
		return "", "", err
	}
	return rewritten, con.GetConname(), nil
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
