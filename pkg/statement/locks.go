package statement

import (
	"slices"
	"strings"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// TableName is a table as a statement spells it: its schema
// qualification, empty when the statement leaves it unqualified, and its
// name.
type TableName struct {
	Schema string
	Name   string
}

// lockShare is PostgreSQL's ShareLock, the weakest table lock mode that
// conflicts with the ROW EXCLUSIVE lock every INSERT, UPDATE, and DELETE
// takes. Each mode is one more than the mode below it.
const lockShare = 5

// TablesLockedAgainstWrites returns the tables a LOCK TABLE or TRUNCATE
// statement locks in a mode that blocks writes, each as the statement
// spells it, and none for any other statement. LOCK TABLE without a mode
// takes ACCESS EXCLUSIVE, SHARE and every stronger mode block writes, and
// the weaker modes block neither reads nor writes. TRUNCATE takes ACCESS
// EXCLUSIVE.
func TablesLockedAgainstWrites(sql string) ([]TableName, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return nil, err
	}
	var relations []*pganalyze.Node
	switch {
	case node.GetLockStmt() != nil:
		if node.GetLockStmt().GetMode() < lockShare {
			return nil, nil
		}
		relations = node.GetLockStmt().GetRelations()
	case node.GetTruncateStmt() != nil:
		relations = node.GetTruncateStmt().GetRelations()
	default:
		return nil, nil
	}
	tables := make([]TableName, 0, len(relations))
	for _, rel := range relations {
		rv := rel.GetRangeVar()
		tables = append(tables, TableName{Schema: rv.GetSchemaname(), Name: rv.GetRelname()})
	}
	return tables, nil
}

// TakesShareUpdateExclusiveOnly reports an ALTER TABLE operation whose
// lock on the table is SHARE UPDATE EXCLUSIVE, which blocks neither reads
// nor writes: VALIDATE CONSTRAINT, SET STATISTICS and a column's SET /
// RESET options, and SET / RESET of the storage parameters PostgreSQL
// changes under that lock. SET STORAGE, user_catalog_table, and a
// parameter not listed here count as blocking.
func (op Op) TakesShareUpdateExclusiveOnly() bool {
	switch op.Kind {
	case OpValidateConstraint:
		return true
	case OpSetColumnOptions:
		return !op.ColumnStorage
	case OpSetRelOptions:
		return len(op.RelOptions) > 0 && !slices.ContainsFunc(op.RelOptions, func(name string) bool {
			return !shareUpdateExclusiveRelOption(name)
		})
	default:
		return false
	}
}

// shareUpdateExclusiveRelOption reports a table storage parameter
// PostgreSQL sets or resets under SHARE UPDATE EXCLUSIVE: every
// autovacuum_ parameter and the ones listed. A toast. prefix changes none
// of them.
func shareUpdateExclusiveRelOption(name string) bool {
	name = strings.TrimPrefix(strings.ToLower(name), "toast.")
	switch name {
	case "fillfactor", "toast_tuple_target", "parallel_workers",
		"vacuum_index_cleanup", "vacuum_truncate", "log_autovacuum_min_duration":
		return true
	default:
		return strings.HasPrefix(name, "autovacuum_")
	}
}
