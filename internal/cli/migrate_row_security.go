package cli

import (
	"context"
	"errors"
	"io"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func (c *MigrateCmd) runDesiredRowSecurity(ctx context.Context, out io.Writer, desired statement.DesiredWithRowSecurity) error {
	b := executor.Budget{LockTimeout: c.LockTimeout, StatementTimeout: c.StatementTimeout}
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	report, planErr := diffplan.PlanWithRowSecurity(ctx, pool, c.Schema, desired)
	diff := DiffCmd{DBFlags: c.DBFlags, OutputFlags: c.OutputFlags, Schema: c.Schema, JSON: c.JSON}
	if c.DryRun {
		if changesOnlyTheTable(report, planErr) {
			// Preview uses desired-state admission, as for a file without
			// row security: RunDesired refuses a destructive plan whole.
			plan.RefuseDestructive(&report)
			return diff.writeDiffReport(out, report)
		}
		return diff.writeRowSecurityDiff(ctx, out, pool, desired, b, report, planErr)
	}
	if runsTheTablePlan(report, planErr) {
		result, runErr := migrate.RunDesiredWithRowSecurity(ctx, pool, migrate.RowSecurityRequest{Schema: c.Schema, Desired: desired}, c.options(c.diag()))
		return c.finishDesired(out, result, runErr)
	}
	runReport, runErr := executor.ExecuteDesiredRowSecurity(ctx, pool, c.Schema, "", desired, b)
	v := rowSecurityVerdict(c.Schema+"."+desired.Table(), runReport, runErr)
	emitErr := c.emit(out, v)
	return errors.Join(emitErr, runErr)
}

// changesOnlyTheTable reports whether planning a row-security file found
// the live row security matching and only the table definition differing,
// the case RunDesiredWithRowSecurity converges. Every other case, a create,
// a row security change, a refusal, or no change at all, goes to the
// atomic row security executor, which derives it again under its lock.
func changesOnlyTheTable(report plan.Report, planErr error) bool {
	return planErr == nil && len(report.Statements) > 0
}

// runsTheTablePlan reports whether the row security matched, so
// RunDesiredWithRowSecurity runs the file: the table definition has a plan,
// or its plan refused the table change, which that path reports in the
// terms a file without row security gets.
func runsTheTablePlan(report plan.Report, planErr error) bool {
	return changesOnlyTheTable(report, planErr) || errors.Is(planErr, schemadiff.ErrOnlyTableDiffers)
}

// A failed commit is not evidence of rollback. Preserve its distinct code and
// explanation; never disclose uncommitted statements as executed SQL.
func rowSecurityVerdict(table string, report executor.RowSecurityReport, err error) verdict.Verdict {
	v := verdict.Verdict{Table: table, Outcome: verdict.OutcomeExecuted, ExecutedSQL: report.Statements,
		Detail: "row security converged: all changes committed in one transaction"}
	if err == nil {
		if report.TableCreated {
			v.TableCreated = true
			v.ExtensionSchemas = report.ExtensionSchemas
			v.Detail = "table created with its row security: all changes committed in one transaction"
		}
		if len(report.Statements) == 0 {
			v.Detail = "already converged: row security matches; nothing to run"
		}
		return v
	}
	v.Outcome = verdict.OutcomeFailed
	v.ExecutedSQL = nil
	v.Detail = err.Error()
	if refusal, ok := migrate.RowSecurityRefusal(err); ok {
		return v.WithRefusal(refusal)
	}
	v.Code = string(executor.OutcomeCode(err))
	return migrate.WithServerError(v, err)
}
