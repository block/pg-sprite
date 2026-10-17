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
func RefreshColumnStatistics(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, columns []string, b Budget) error {
	if len(columns) == 0 {
		return errors.New("refresh column statistics: no columns named")
	}
	if err := b.validate(); err != nil {
		return fmt.Errorf("refresh column statistics: %w", err)
	}
	cols := make([]string, len(columns))
	for i, c := range columns {
		cols[i] = pgx.Identifier{c}.Sanitize()
	}
	table := pgx.Identifier{pt.Table()}.Sanitize()
	if pt.Schema() != "" {
		table = pgx.Identifier{pt.Schema(), pt.Table()}.Sanitize()
	}
	analyze := "ANALYZE " + table + " (" + strings.Join(cols, ", ") + ")"

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
