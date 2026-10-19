package statement

import (
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// TransactionControl is what a statement does to an explicit transaction
// block, read from the parse tree.
type TransactionControl int

// The effects a statement can have on an explicit transaction block.
const (
	// TransactionNone: the statement neither opens nor closes a block. A
	// savepoint statement, and a COMMIT or ROLLBACK AND CHAIN, which
	// closes one block and opens the next at once, leave a script inside
	// its block.
	TransactionNone TransactionControl = iota
	// TransactionBegin: BEGIN or START TRANSACTION opens a block.
	TransactionBegin
	// TransactionEnd: COMMIT, END, ROLLBACK, ABORT, or PREPARE
	// TRANSACTION closes the open block.
	TransactionEnd
)

// TransactionControlOf reports what sql, exactly one statement, does to an
// explicit transaction block.
func TransactionControlOf(sql string) (TransactionControl, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return TransactionNone, fmt.Errorf("parse statement: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return TransactionNone, fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	tx := tree.GetStmts()[0].GetStmt().GetTransactionStmt()
	if tx == nil {
		return TransactionNone, nil
	}
	switch tx.GetKind() {
	case pganalyze.TransactionStmtKind_TRANS_STMT_BEGIN, pganalyze.TransactionStmtKind_TRANS_STMT_START:
		return TransactionBegin, nil
	case pganalyze.TransactionStmtKind_TRANS_STMT_COMMIT, pganalyze.TransactionStmtKind_TRANS_STMT_ROLLBACK:
		if tx.GetChain() {
			return TransactionNone, nil
		}
		return TransactionEnd, nil
	case pganalyze.TransactionStmtKind_TRANS_STMT_PREPARE:
		return TransactionEnd, nil
	default:
		return TransactionNone, nil
	}
}
