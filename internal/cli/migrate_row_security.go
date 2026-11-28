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
	if c.AcceptBlocking != "" {
		return errors.New("--accept-blocking does not apply to a file that declares row security; " +
			"drop a partitioned table's index with a desired file for the table definition alone")
	}
	b := executor.Budget{LockTimeout: c.LockTimeout, StatementTimeout: c.StatementTimeout}
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	report, planErr := diffplan.PlanWithRowSecurity(ctx, pool, c.Schema, desired)
	diff := DiffCmd{DBFlags: c.DBFlags, OutputFlags: c.OutputFlags, Schema: c.Schema, JSON: c.JSON}
	if c.DryRun {
		if changesOnlyTheTable(report, planErr) || c.pinnedAndConverged(report, planErr) {
			// Preview uses desired-state admission, as for a file without
			// row security.
			return c.previewDesired(ctx, pool, out, report)
		}
		if errors.Is(planErr, schemadiff.ErrOnlyTableDiffers) {
			// Only the table definition differs and its plan refused; the
			// flags belong to that change, so the refusal is shown as is.
			return diff.writeRowSecurityDiff(ctx, out, pool, desired, b, report, planErr)
		}
		if err := c.rejectPinOnAtomicRowSecurity(); err != nil {
			return err
		}
		// The run refuses the change while a copy-and-swap run on the table
		// is stopped, and so does the dry run, in place of the review.
		v, held, err := migrate.AdmitRowSecurityStoppedRun(ctx, pool, c.Schema, desired.Table())
		if err != nil {
			return err
		}
		if held {
			return c.emit(out, v)
		}
		return diff.writeRowSecurityDiff(ctx, out, pool, desired, b, report, planErr)
	}
	if runsTheTablePlan(report, planErr) || c.pinnedAndConverged(report, planErr) {
		opts, stopProgress, err := c.startProgress(ctx, c.desiredOptions(c.diag()))
		if err != nil {
			return err
		}
		result, runErr := migrate.RunDesiredWithRowSecurity(ctx, pool, migrate.RowSecurityRequest{
			Schema:              c.Schema,
			Desired:             desired,
			ExpectedFingerprint: c.ExpectFingerprint,
			AllowDestructive:    c.AllowDestructive,
		}, opts)
		stopProgress()
		return c.finishDesired(out, result, runErr)
	}
	if err := c.rejectPinOnAtomicRowSecurity(); err != nil {
		return err
	}
	// The atomic change runs no statement a stopped copy-and-swap run
	// records, so a run stopped on the table refuses it before it locks
	// the table.
	refusal, held, err := migrate.AdmitRowSecurityStoppedRun(ctx, pool, c.Schema, desired.Table())
	if err != nil {
		return err
	}
	if held {
		return c.emit(out, refusal)
	}
	runReport, runErr := executor.ExecuteDesiredRowSecurity(ctx, pool, c.Schema, "", desired, b)
	v := rowSecurityVerdict(c.Schema+"."+desired.Table(), runReport, runErr)
	emitErr := c.emit(out, v)
	return errors.Join(emitErr, runErr)
}

// rejectPinOnAtomicRowSecurity refuses a pin or a destructive
// acknowledgement on a change that runs through the atomic row security
// executor, which derives its changes again under its own lock and has no
// plan to pin: accepting either flag would let a run ignore the review the
// operator thinks it is bound to.
func (c *MigrateCmd) rejectPinOnAtomicRowSecurity() error {
	switch {
	case c.ExpectFingerprint != "":
		return errors.New("--expect-fingerprint does not apply to this file: its row security change runs in one transaction under its own lock and has no plan to pin; pin a change to the table definition alone")
	case c.AllowDestructive != "":
		return errors.New("--allow-destructive does not apply to this file: its row security change runs in one transaction and acknowledges nothing; it applies to a change to the table definition alone")
	}
	return nil
}

// pinnedAndConverged reports whether a run that carries the pin or the
// destructive acknowledgement finds the whole file already matching, as it
// does when the same reviewed command runs again after its table change
// converged. That run goes through the desired-state checks, which accept
// an empty plan, rather than to the atomic executor, which takes no flags.
func (c *MigrateCmd) pinnedAndConverged(report plan.Report, planErr error) bool {
	flagged := c.ExpectFingerprint != "" || c.AllowDestructive != ""
	return flagged && planErr == nil && len(report.Statements) == 0
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
		if errors.Is(err, executor.ErrRowSecurityTableShape) {
			v.Detail = rowSecurityShapeNextStep(err)
		}
		return v.WithRefusal(refusal)
	}
	v.Code = string(executor.OutcomeCode(err))
	return migrate.WithServerError(v, err)
}
