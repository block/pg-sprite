// Package statement parses SQL through the real PostgreSQL grammar
// (wasilibs/go-pgquery, Wasm libpg_query) and reports the facts the engine's
// front door needs. In Phase 1 that is a statement-type gate only: which kind
// of statement this is and, for ALTER TABLE, which table it targets. No
// schema model, no classification.
package statement

import (
	"errors"
	"fmt"
	"strings"

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
	KindCreateTable
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
	case KindCreateTable:
		return "CREATE TABLE"
	default:
		return "other"
	}
}

// Statement is one parsed SQL statement plus the facts the gate needs. It can
// only be constructed by ParseOne, so holding one proves the SQL parsed as
// exactly one statement through the PostgreSQL grammar — the proof the
// executor requires before running anything (invariant ST-7).
type Statement struct {
	sql        string
	kind       Kind
	schema     string
	table      string
	concurrent bool
}

// SQL returns the original statement text as submitted.
func (s Statement) SQL() string { return s.sql }

// Kind returns the statement-type bucket.
func (s Statement) Kind() Kind { return s.kind }

// Schema returns the target table's schema qualification for ALTER TABLE
// statements; empty when the statement was unqualified (search_path resolves
// it) or when the kind has no single table target.
func (s Statement) Schema() string { return s.schema }

// Table returns the target table name for ALTER TABLE statements; empty for
// other kinds.
func (s Statement) Table() string { return s.table }

// Concurrent reports whether an index statement used its CONCURRENTLY form.
// It is always false for non-index kinds.
func (s Statement) Concurrent() bool { return s.concurrent }

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
	st := Statement{sql: sql}
	node := tree.GetStmts()[0].GetStmt()
	switch {
	case node.GetAlterTableStmt() != nil:
		alter := node.GetAlterTableStmt()
		// ALTER INDEX (and ALTER VIEW etc.) also parse as AlterTableStmt;
		// only a true table target is KindAlterTable.
		if alter.GetObjtype() != pganalyze.ObjectType_OBJECT_TABLE {
			return st, nil
		}
		st.kind = KindAlterTable
		st.schema = alter.GetRelation().GetSchemaname()
		st.table = alter.GetRelation().GetRelname()
	case node.GetCreateStmt() != nil:
		rel := node.GetCreateStmt().GetRelation()
		st.kind = KindCreateTable
		st.schema = rel.GetSchemaname()
		st.table = rel.GetRelname()
	case node.GetRenameStmt() != nil:
		// ALTER TABLE ... RENAME TO / RENAME COLUMN / RENAME CONSTRAINT
		// parse as RenameStmt, not AlterTableStmt. Only table-targeted
		// renames are KindAlterTable: RENAME TO carries OBJECT_TABLE as the
		// rename type, RENAME COLUMN carries it as the relation type, and
		// RENAME CONSTRAINT carries the table-specific OBJECT_TABCONSTRAINT.
		// ALTER INDEX/VIEW ... RENAME carry their own object types and stay
		// KindOther.
		ren := node.GetRenameStmt()
		if ren.GetRenameType() != pganalyze.ObjectType_OBJECT_TABLE &&
			ren.GetRenameType() != pganalyze.ObjectType_OBJECT_TABCONSTRAINT &&
			ren.GetRelationType() != pganalyze.ObjectType_OBJECT_TABLE {
			return st, nil
		}
		st.kind = KindAlterTable
		st.schema = ren.GetRelation().GetSchemaname()
		st.table = ren.GetRelation().GetRelname()
	case node.GetAlterObjectSchemaStmt() != nil:
		// ALTER TABLE ... SET SCHEMA parses as AlterObjectSchemaStmt; only
		// the table-targeted form is KindAlterTable.
		move := node.GetAlterObjectSchemaStmt()
		if move.GetObjectType() != pganalyze.ObjectType_OBJECT_TABLE {
			return st, nil
		}
		st.kind = KindAlterTable
		st.schema = move.GetRelation().GetSchemaname()
		st.table = move.GetRelation().GetRelname()
	case node.GetIndexStmt() != nil:
		st.kind = KindCreateIndex
		st.concurrent = node.GetIndexStmt().GetConcurrent()
	case node.GetDropStmt() != nil:
		if node.GetDropStmt().GetRemoveType() == pganalyze.ObjectType_OBJECT_INDEX {
			st.kind = KindDropIndex
			st.concurrent = node.GetDropStmt().GetConcurrent()
		}
	case node.GetReindexStmt() != nil:
		st.kind = KindReindex
		st.concurrent = reindexConcurrently(node.GetReindexStmt())
	}
	return st, nil
}

// reindexConcurrently reports whether a REINDEX statement used its
// CONCURRENTLY form, which the grammar carries as a generic option rather
// than a dedicated field.
func reindexConcurrently(stmt *pganalyze.ReindexStmt) bool {
	for _, p := range stmt.GetParams() {
		if strings.EqualFold(p.GetDefElem().GetDefname(), "concurrently") {
			return true
		}
	}
	return false
}
