package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
)

// ErrAnalyzeNotPermitted means the connected role cannot act as the owner
// of a table the refresh covers, so ANALYZE would skip it with only a
// warning and leave its statistics as they were. Nothing was analyzed.
var ErrAnalyzeNotPermitted = errors.New("the role cannot act as the table's owner, so ANALYZE would skip it")

// ErrAnalyzeForeignChild means a table the refresh covers is a foreign
// table. Whether ANALYZE can sample one depends on its foreign-data
// wrapper, and a wrapper that cannot is skipped with only a warning, so
// the refresh refuses rather than report statistics it may never have
// refreshed. Nothing was analyzed.
var ErrAnalyzeForeignChild = errors.New("an inheritance child is a foreign table, which ANALYZE may skip")

// RefreshTableStatistics runs ANALYZE on the preflighted table and on
// every table that inherits from it, in one transaction under
// transaction-local budgets. It returns the ANALYZE it ran, or, with an
// error, the one an operator still has to run.
//
// ALTER COLUMN TYPE deletes the column's planner statistics, for a catalog
// relabel as much as for a rewrite, and nothing restores them until
// autovacuum's change threshold trips. On a table that changes slowly that
// can be a long time, and until then the planner estimates every predicate
// on the column from defaults. ANALYZE samples rather than scans, and its
// SHARE UPDATE EXCLUSIVE lock blocks neither reads nor writes; it waits only
// behind another ANALYZE, VACUUM, or DDL on the table, which the lock budget
// bounds.
//
// The whole table is analyzed rather than the retyped columns alone: the
// server computes expression-index statistics only for a whole-table
// ANALYZE, and an extended statistics object only when every column it
// covers is sampled, and a type change can rebuild or invalidate either. A
// type change on an inheritance parent recurses into its children and
// deletes their statistics too, while ANALYZE on the parent does not
// analyze them, so each child is named; a partitioned table's partitions
// need no naming, since ANALYZE on the parent recurses into them itself.
//
// The server skips a table whose owner the role cannot act as with only a
// warning, so a refresh that completed proves nothing on its own. The
// refresh checks ownership of every table it covers first, in the same
// transaction, and returns ErrAnalyzeNotPermitted rather than report
// statistics it never refreshed. Ownership is the privilege every native
// step on the table needs anyway; a role ANALYZE admits on other grounds is
// refused here, which costs a manual ANALYZE and never a false report.
func RefreshTableStatistics(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, b Budget) (string, error) {
	analyze := analyzeSQL([]string{quotedTable(pt)})
	if err := b.validate(); err != nil {
		return analyze, fmt.Errorf("refresh table statistics: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return analyze, fmt.Errorf("begin statistics refresh: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	// INV: LK-2 — the refresh runs under the same transaction-local
	// budgets as every native step.
	setBudgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, setBudgets); err != nil {
		return analyze, fmt.Errorf("set statistics refresh budgets: %w", err)
	}
	tables, err := readAnalyzeTree(ctx, tx, pt)
	if err != nil {
		return analyze, err
	}
	names := make([]string, len(tables))
	for i, tbl := range tables {
		names[i] = tbl.name
	}
	analyze = analyzeSQL(names)
	for _, tbl := range tables {
		if tbl.foreign {
			return analyze, fmt.Errorf("refresh the statistics of %s: %s: %w", names[0], tbl.name, ErrAnalyzeForeignChild)
		}
		if !tbl.owned {
			return analyze, fmt.Errorf("refresh the statistics of %s: %w", tbl.name, ErrAnalyzeNotPermitted)
		}
	}
	if _, err := tx.Exec(ctx, analyze); err != nil {
		if budgetErr := asBudgetError(err, b); budgetErr != nil {
			return analyze, fmt.Errorf("%s: %w", analyze, budgetErr)
		}
		return analyze, fmt.Errorf("%s: %w", analyze, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return analyze, fmt.Errorf("commit statistics refresh: %w", err)
	}
	return analyze, nil
}

// analyzeTable is one table a statistics refresh covers.
type analyzeTable struct {
	// name is the schema-qualified name, every identifier quoted.
	name string
	// owned reports that the connected role has the owner's privileges.
	owned bool
	// foreign reports a foreign table.
	foreign bool
}

// readAnalyzeTree returns the preflighted table followed by every table
// that inherits from it, directly or through another child, parents before
// their children. The walk does not descend from a partitioned table,
// whose partitions ANALYZE on it covers already.
func readAnalyzeTree(ctx context.Context, tx pgx.Tx, pt preflight.PreflightedTable) ([]analyzeTable, error) {
	root := quotedTable(pt)
	rows, err := tx.Query(ctx, `WITH RECURSIVE tree(oid, depth) AS (
			SELECT $1::regclass::oid, 0
			UNION
			SELECT i.inhrelid, tree.depth + 1
			FROM tree
			JOIN pg_catalog.pg_inherits i ON i.inhparent = tree.oid
			JOIN pg_catalog.pg_class p ON p.oid = i.inhparent
			WHERE p.relkind <> 'p'
		)
		SELECT n.nspname, c.relname,
			pg_catalog.pg_has_role(current_user, c.relowner, 'USAGE'),
			c.relkind = 'f'
		FROM tree
		JOIN pg_catalog.pg_class c ON c.oid = tree.oid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		ORDER BY tree.depth, n.nspname, c.relname`, root)
	if err != nil {
		return nil, fmt.Errorf("read the tables an ANALYZE of %s covers: %w", root, err)
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (analyzeTable, error) {
		var schema, table string
		var tbl analyzeTable
		if err := row.Scan(&schema, &table, &tbl.owned, &tbl.foreign); err != nil {
			return analyzeTable{}, err
		}
		tbl.name = pgx.Identifier{schema, table}.Sanitize()
		return tbl, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the tables an ANALYZE of %s covers: %w", root, err)
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("read the tables an ANALYZE of %s covers: the table is gone", root)
	}
	return tables, nil
}

// analyzeSQL renders the ANALYZE of the named tables, each already quoted.
func analyzeSQL(tables []string) string {
	return "ANALYZE " + strings.Join(tables, ", ")
}

// quotedTable is the preflighted table's name, schema-qualified when the
// proof carries a schema, every identifier quoted.
func quotedTable(pt preflight.PreflightedTable) string {
	if pt.Schema() != "" {
		return pgx.Identifier{pt.Schema(), pt.Table()}.Sanitize()
	}
	return pgx.Identifier{pt.Table()}.Sanitize()
}
