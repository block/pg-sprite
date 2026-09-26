package cli

import (
	"context"
	"errors"
	"io"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func (c *MigrateCmd) runDesiredRowSecurity(ctx context.Context, out io.Writer, sql string) error {
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	if err != nil {
		return err
	}
	if c.DryRun {
		diff := DiffCmd{DBFlags: c.DBFlags, OutputFlags: c.OutputFlags, Schema: c.Schema, JSON: c.JSON}
		return diff.runRowSecurityDiff(ctx, out, sql)
	}
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	report, runErr := executor.ExecuteRowSecurity(ctx, pool, c.Schema, desired, executor.Budget{
		LockTimeout: c.LockTimeout, StatementTimeout: c.StatementTimeout,
	})
	v := rowSecurityVerdict(c.Schema+"."+desired.Table(), report, runErr)
	emitErr := c.emit(out, v)
	return errors.Join(emitErr, runErr)
}

// A failed commit is not evidence of rollback. Preserve its distinct code and
// explanation; never disclose uncommitted statements as executed SQL.
func rowSecurityVerdict(table string, report executor.RowSecurityReport, err error) verdict.Verdict {
	v := verdict.Verdict{Table: table, Outcome: verdict.OutcomeExecuted, ExecutedSQL: report.Statements,
		Detail: "row security converged: all changes committed in one transaction"}
	if err == nil {
		if len(report.Statements) == 0 {
			v.Detail = "already converged: row security matches; nothing to run"
		}
		return v
	}
	v.Outcome = verdict.OutcomeFailed
	v.ExecutedSQL = nil
	v.Detail = err.Error()
	if errors.Is(err, executor.ErrRowSecurityRefused) {
		return v.WithRefusal(verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement))
	}
	if errors.Is(err, executor.ErrTableNotFound) {
		return v.WithRefusal(verdict.Environmental(verdict.ReasonUnsupportedStatement))
	}
	v.Code = string(executor.OutcomeCode(err))
	return v
}
