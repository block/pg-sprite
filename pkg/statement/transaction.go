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
	// TransactionNone: the statement neither opens nor closes a block,
	// and sets no savepoint.
	TransactionNone TransactionControl = iota
	// TransactionBegin: BEGIN or START TRANSACTION opens a block.
	TransactionBegin
	// TransactionEnd: COMMIT, END, ROLLBACK, ABORT, or PREPARE
	// TRANSACTION closes the open block.
	TransactionEnd
	// TransactionChain: COMMIT or ROLLBACK AND CHAIN closes one block and
	// opens the next at once. The script stays inside a block, but the
	// locks the closed block held are released.
	TransactionChain
	// TransactionSavepoint: SAVEPOINT marks a point inside the block.
	TransactionSavepoint
	// TransactionRelease: RELEASE SAVEPOINT forgets a savepoint and every
	// later one; the locks taken since stay held.
	TransactionRelease
	// TransactionRollbackTo: ROLLBACK TO SAVEPOINT undoes the work since
	// a savepoint and releases the locks taken since it; the savepoint
	// stays, and every later one is forgotten.
	TransactionRollbackTo
)

// Transaction is what one statement does to an explicit transaction
// block, with the savepoint it names, if any.
type Transaction struct {
	Control TransactionControl
	// Savepoint is the savepoint a TransactionSavepoint,
	// TransactionRelease, or TransactionRollbackTo names.
	Savepoint string
}

// TransactionControlOf reports what sql, exactly one statement, does to an
// explicit transaction block.
func TransactionControlOf(sql string) (Transaction, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return Transaction{}, fmt.Errorf("parse statement: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return Transaction{}, fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	tx := tree.GetStmts()[0].GetStmt().GetTransactionStmt()
	if tx == nil {
		return Transaction{}, nil
	}
	switch tx.GetKind() {
	case pganalyze.TransactionStmtKind_TRANS_STMT_BEGIN, pganalyze.TransactionStmtKind_TRANS_STMT_START:
		return Transaction{Control: TransactionBegin}, nil
	case pganalyze.TransactionStmtKind_TRANS_STMT_COMMIT, pganalyze.TransactionStmtKind_TRANS_STMT_ROLLBACK:
		if tx.GetChain() {
			return Transaction{Control: TransactionChain}, nil
		}
		return Transaction{Control: TransactionEnd}, nil
	case pganalyze.TransactionStmtKind_TRANS_STMT_PREPARE:
		return Transaction{Control: TransactionEnd}, nil
	case pganalyze.TransactionStmtKind_TRANS_STMT_SAVEPOINT:
		return Transaction{Control: TransactionSavepoint, Savepoint: tx.GetSavepointName()}, nil
	case pganalyze.TransactionStmtKind_TRANS_STMT_RELEASE:
		return Transaction{Control: TransactionRelease, Savepoint: tx.GetSavepointName()}, nil
	case pganalyze.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO:
		return Transaction{Control: TransactionRollbackTo, Savepoint: tx.GetSavepointName()}, nil
	default:
		return Transaction{}, nil
	}
}
