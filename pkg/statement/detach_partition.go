package statement

import pganalyze "github.com/pganalyze/pg_query_go/v6"

// DetachedPartition returns the partition a single-command ALTER TABLE ...
// DETACH PARTITION names, as the grammar spelled it: its schema (empty when
// the name is unqualified) and its name. ok is false for every other
// statement, a DETACH PARTITION ... FINALIZE included.
func DetachedPartition(sql string) (schema, name string, ok bool, err error) {
	node, err := parseSingle(sql)
	if err != nil {
		return "", "", false, err
	}
	cmd := detachPartitionCmd(node)
	if cmd == nil {
		return "", "", false, nil
	}
	return cmd.GetName().GetSchemaname(), cmd.GetName().GetRelname(), true, nil
}

// FinalizesDetach reports whether the statement is an ALTER TABLE with a
// DETACH PARTITION ... FINALIZE subcommand, the statement that finishes a
// concurrent detach that was interrupted. The deparser does not reprint it
// faithfully: it drops the space before FINALIZE, which turns the keyword
// into part of the partition's name, so a front door has to refuse it
// before it reprints the statement.
func (s Statement) FinalizesDetach() bool { return s.finalizesDetach }

// alterFinalizesDetach reports whether alter has a DETACH PARTITION ...
// FINALIZE subcommand.
func alterFinalizesDetach(alter *pganalyze.AlterTableStmt) bool {
	for _, cmd := range alter.GetCmds() {
		if cmd.GetAlterTableCmd().GetSubtype() == pganalyze.AlterTableType_AT_DetachPartitionFinalize {
			return true
		}
	}
	return false
}
