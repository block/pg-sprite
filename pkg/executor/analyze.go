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

// ErrAnalyzeNotPermitted means the connected role cannot act as the table's
// owner, so ANALYZE would skip the table with only a warning and leave the
// statistics as they were. Nothing was analyzed.
var ErrAnalyzeNotPermitted = errors.New("the role cannot act as the table's owner, so ANALYZE would skip it")

// RefreshColumnStatistics runs ANALYZE on the named columns of the
// preflighted table, in one transaction under transaction-local budgets.
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
// The server skips a table whose owner the role cannot act as with only a
// warning, so a refresh that completed proves nothing on its own. The
// refresh checks ownership first, in the same transaction, and returns
// ErrAnalyzeNotPermitted rather than report statistics it never refreshed.
// Ownership is the privilege every native step on the table needs anyway;
// a role ANALYZE admits on other grounds is refused here, which costs a
// manual ANALYZE and never a false report.
func RefreshColumnStatistics(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, columns []string, b Budget) error {
	if len(columns) == 0 {
		return errors.New("refresh column statistics: no columns named")
	}
	if err := b.validate(); err != nil {
		return fmt.Errorf("refresh column statistics: %w", err)
	}
	analyze := AnalyzeSQL(pt, columns)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin statistics refresh: %w", err)
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
		return fmt.Errorf("set statistics refresh budgets: %w", err)
	}
	if err := checkAnalyzePermitted(ctx, tx, pt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, analyze); err != nil {
		if budgetErr := asBudgetError(err, b); budgetErr != nil {
			return fmt.Errorf("%s: %w", analyze, budgetErr)
		}
		return fmt.Errorf("%s: %w", analyze, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit statistics refresh: %w", err)
	}
	return nil
}

// AnalyzeSQL renders the ANALYZE that refreshes the named columns of the
// preflighted table, every identifier quoted, or the whole table when no
// column is named. It is the statement RefreshColumnStatistics runs and the
// one an operator is told to run when the refresh could not.
func AnalyzeSQL(pt preflight.PreflightedTable, columns []string) string {
	table := quotedTable(pt)
	if len(columns) == 0 {
		return "ANALYZE " + table
	}
	cols := make([]string, len(columns))
	for i, c := range columns {
		cols[i] = pgx.Identifier{c}.Sanitize()
	}
	return "ANALYZE " + table + " (" + strings.Join(cols, ", ") + ")"
}

// checkAnalyzePermitted refuses unless the connected role has the
// privileges of the preflighted table's owner, which a superuser always
// has.
func checkAnalyzePermitted(ctx context.Context, tx pgx.Tx, pt preflight.PreflightedTable) error {
	table := quotedTable(pt)
	var owner bool
	err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_has_role(current_user, c.relowner, 'USAGE')
		FROM pg_catalog.pg_class c WHERE c.oid = $1::regclass`, table).Scan(&owner)
	if err != nil {
		return fmt.Errorf("check the owner of %s before ANALYZE: %w", table, err)
	}
	if !owner {
		return fmt.Errorf("refresh the statistics of %s: %w", table, ErrAnalyzeNotPermitted)
	}
	return nil
}

// quotedTable is the preflighted table's name, schema-qualified when the
// proof carries a schema, every identifier quoted.
func quotedTable(pt preflight.PreflightedTable) string {
	if pt.Schema() != "" {
		return pgx.Identifier{pt.Schema(), pt.Table()}.Sanitize()
	}
	return pgx.Identifier{pt.Table()}.Sanitize()
}
