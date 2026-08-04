// Package statement parses SQL through the real PostgreSQL grammar
// (wasilibs/go-pgquery, Wasm libpg_query) and reports the facts the engine's
// front door needs. In Phase 1 that is a statement-type gate only: which kind
// of statement this is and, for ALTER TABLE, which table it targets. No
// schema model, no classification.
package statement

import (
	"errors"
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// Kind is the statement-type bucket the Phase 1 gate branches on.
type Kind int

// The kinds the gate distinguishes. Everything the engine does not recognize
// as one of the named kinds is KindOther and is refused by the front door.
const (
	KindOther Kind = iota
	KindAlterTable
	KindCreateIndex
	KindDropIndex
	KindReindex
)

// String returns the human-readable name of the kind.
func (k Kind) String() string {
	switch k {
	case KindAlterTable:
		return "ALTER TABLE"
	case KindCreateIndex:
		return "CREATE INDEX"
	case KindDropIndex:
		return "DROP INDEX"
	case KindReindex:
		return "REINDEX"
	default:
		return "other"
	}
}

// Statement is one parsed SQL statement plus the facts the gate needs.
type Statement struct {
	// SQL is the original statement text as submitted.
	SQL string
	// Kind is the statement-type bucket.
	Kind Kind
	// Schema is the target table's schema qualification for ALTER TABLE
	// statements; empty when the statement was unqualified (search_path
	// resolves it) or when the kind has no single table target.
	Schema string
	// Table is the target table name for ALTER TABLE statements; empty for
	// other kinds.
	Table string
}

// ErrNotOneStatement is returned by ParseOne when the input does not contain
// exactly one SQL statement.
var ErrNotOneStatement = errors.New("input must contain exactly one SQL statement")

// ParseOne parses sql with the PostgreSQL grammar and requires exactly one
// statement. A parse failure is surfaced to the caller, never guessed around.
func ParseOne(sql string) (Statement, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return Statement{}, fmt.Errorf("parse statement: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return Statement{}, fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	st := Statement{SQL: sql}
	node := tree.GetStmts()[0].GetStmt()
	switch {
	case node.GetAlterTableStmt() != nil:
		alter := node.GetAlterTableStmt()
		// ALTER INDEX (and ALTER VIEW etc.) also parse as AlterTableStmt;
		// only a true table target is KindAlterTable.
		if alter.GetObjtype() != pganalyze.ObjectType_OBJECT_TABLE {
			return st, nil
		}
		st.Kind = KindAlterTable
		st.Schema = alter.GetRelation().GetSchemaname()
		st.Table = alter.GetRelation().GetRelname()
	case node.GetIndexStmt() != nil:
		st.Kind = KindCreateIndex
	case node.GetDropStmt() != nil:
		if node.GetDropStmt().GetRemoveType() == pganalyze.ObjectType_OBJECT_INDEX {
			st.Kind = KindDropIndex
		}
	case node.GetReindexStmt() != nil:
		st.Kind = KindReindex
	}
	return st, nil
}
