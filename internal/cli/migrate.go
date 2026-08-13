package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// run is the migrate flow: gate the statement type, size-guard the table,
// attempt the change under budget, and end in exactly one verdict. Refusal
// verdicts are printed to out and returned as verdict.ErrRefused so the entry
// point maps them to the refusal exit code. --dry-run diverts to the
// classify-and-route plan instead.
func (c *MigrateCmd) run(ctx context.Context, out io.Writer) error {
	if c.DryRun {
		return c.runDryRun(ctx, out)
	}
	logger := c.diag()
	st, err := statement.ParseOne(c.Alter)
	if err != nil {
		return err
	}
	logger.Debug("statement parsed", "kind", st.Kind(), "schema", st.Schema(), "table", st.Table())
	if v, refused := gateVerdict(st); refused {
		return c.emit(out, v)
	}

	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	// The native path is in-place ALTER TABLE: owner-gated, no WAL
	// decoding — Tier 1 of the engine-role contract.
	priv, err := preflight.CheckPrivileges(ctx, pool, st.Schema(), st.Table(),
		preflight.Requirement{Tier: preflight.TierAlterInPlace})
	var privErr *preflight.PrivilegeError
	if errors.As(err, &privErr) {
		return c.emit(out, privilegeVerdict(st, privErr))
	}
	if err != nil {
		return err
	}
	logger.Debug("privilege preflight passed", "role", priv.Role(), "owner", priv.Owner())

	pt, err := preflight.CheckTable(ctx, pool, st.Schema(), st.Table(), int64(c.MaxTableSize))
	var sizeErr *preflight.SizeError
	if errors.As(err, &sizeErr) {
		return c.emit(out, sizeGuardVerdict(st, sizeErr))
	}
	if err != nil {
		return err
	}
	logger.Debug("preflight passed",
		"table", qualified(st), "total_bytes", pt.TotalBytes(), "limit_bytes", int64(c.MaxTableSize))

	budget := executor.Budget{LockTimeout: c.LockTimeout, StatementTimeout: c.StatementTimeout}
	retry := c.retryPolicy()
	start := time.Now()
	err = executor.ExecuteNative(ctx, pool, pt, st, budget, retry)
	elapsed := time.Since(start)
	var budgetErr *executor.BudgetError
	if errors.As(err, &budgetErr) {
		logger.Debug("optimistic attempt cancelled",
			"cause", budgetErr.Cause, "budget", budgetErr.Budget, "attempts", budgetErr.Attempts, "elapsed", elapsed)
		return c.emit(out, budgetVerdict(st, budgetErr))
	}
	if err != nil {
		return err
	}
	logger.Debug("optimistic attempt committed", "table", qualified(st), "elapsed", elapsed)
	return c.emit(out, verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: fmt.Sprintf("committed within budgets (lock %s, statement %s): the change was effectively instant",
			budget.LockTimeout, budget.StatementTimeout),
	})
}

func (c *MigrateCmd) retryPolicy() executor.RetryPolicy {
	// Programmatic callers do not pass through Kong's default population.
	// Preserve the safe defaults for a zero-valued command while rejecting
	// partially configured or invalid policies in the executor.
	if c.LockAttempts == 0 && c.LockBackoff == 0 && c.LockBackoffMax == 0 {
		return executor.DefaultRetryPolicy()
	}
	return executor.RetryPolicy{MaxAttempts: c.LockAttempts, InitialBackoff: c.LockBackoff, MaxBackoff: c.LockBackoffMax}
}

// emit prints the verdict in the selected format and returns ErrRefused for
// refusals so the exit code distinguishes them from operational errors.
func (c *MigrateCmd) emit(out io.Writer, v verdict.Verdict) error {
	text := v.String()
	if c.JSON {
		var err error
		if text, err = v.JSON(); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, text); err != nil {
		return fmt.Errorf("write verdict: %w", err)
	}
	if v.Outcome == verdict.OutcomeRefused {
		return verdict.ErrRefused
	}
	return nil
}

// gateVerdict is the Phase 1 statement-type gate: only ALTER TABLE proceeds;
// index maintenance is pointed at its concurrent idiom, everything else is
// unsupported. Refused statements are never executed.
func gateVerdict(st statement.Statement) (verdict.Verdict, bool) {
	v := verdict.Verdict{Outcome: verdict.OutcomeRefused, Statement: st.SQL()}
	switch st.Kind() {
	case statement.KindAlterTable:
		return verdict.Verdict{}, false
	case statement.KindCreateIndex, statement.KindDropIndex, statement.KindReindex:
		v.Reason = verdict.ReasonIndexStatement
		v.Detail, v.SaferIdiom = indexAdvice(st)
	case statement.KindCreateTable:
		v.Reason = verdict.ReasonUnsupportedStatement
		v.Detail = "migrate changes an existing table; to converge a table onto a desired-state CREATE TABLE, use the declarative front-end"
		v.SaferIdiom = "pg-sprite diff --desired schema.sql"
	case statement.KindOther:
		v.Reason = verdict.ReasonUnsupportedStatement
		v.Detail = "only ALTER TABLE statements are supported by the optimistic front door"
	}
	return v, true
}

// indexAdvice explains an index-statement refusal. The already-concurrent
// forms carry no safer idiom: suggesting the statement the user submitted
// would confuse a human once and send a resubmitting automation into a loop.
func indexAdvice(st statement.Statement) (detail, saferIdiom string) {
	if st.Concurrent() {
		return "this is already the safe concurrent idiom; pg-sprite does not drive index maintenance yet — run it directly against the database", ""
	}
	switch st.Kind() {
	case statement.KindCreateIndex:
		return "a plain CREATE INDEX blocks writes for the whole build; the concurrent build does not", "CREATE INDEX CONCURRENTLY"
	case statement.KindDropIndex:
		return "a plain DROP INDEX takes ACCESS EXCLUSIVE on the table; the concurrent drop does not", "DROP INDEX CONCURRENTLY"
	case statement.KindReindex:
		return "a plain REINDEX blocks writes; the concurrent rebuild does not", "REINDEX ... CONCURRENTLY"
	default:
		return "", ""
	}
}

// privilegeVerdict is the refusal for a connected role that lacks the access
// the change needs. The error already names the failed catalog check and the
// exact provisioning statement, so it is the detail verbatim.
func privilegeVerdict(st statement.Statement, privErr *preflight.PrivilegeError) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonInsufficientPrivileges,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    privErr.Error(),
	}
}

// sizeGuardVerdict is the refusal for tables above the size threshold, where
// even a budget-bounded attempt would visibly stall the table.
func sizeGuardVerdict(st statement.Statement, sizeErr *preflight.SizeError) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonTableTooLarge,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: fmt.Sprintf("table is %d bytes on disk (heap, indexes, and TOAST), above the %d-byte "+
			"--max-table-size threshold. pg-sprite cannot yet prove this change is instant on a table this "+
			"size; if it requires a rewrite, a cancelled attempt is not a free probe — it would hold "+
			"ACCESS EXCLUSIVE doing rewrite work for the whole budget",
			sizeErr.TotalBytes, sizeErr.LimitBytes),
	}
}

// budgetVerdict is the refusal for an attempt that exceeded its lock or
// statement budget and was cancelled without executing.
func budgetVerdict(st statement.Statement, budgetErr *executor.BudgetError) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonBudgetExceeded,
		Statement: st.SQL(),
		Table:     qualified(st),
	}
	switch budgetErr.Cause {
	case executor.CauseLock:
		v.Cause = verdict.CauseLockBudget
		v.Attempts = budgetErr.Attempts
		if budgetErr.Attempts > 1 {
			v.Detail = fmt.Sprintf("the lock was not granted within the %s lock budget on any of %d bounded "+
				"attempts: the table is too contended for a blind attempt; nothing was executed",
				budgetErr.Budget, budgetErr.Attempts)
			break
		}
		v.Detail = fmt.Sprintf("the lock was not granted within the %s lock budget: the table is too "+
			"contended for a blind attempt; nothing was executed", budgetErr.Budget)
	case executor.CauseStatement:
		v.Cause = verdict.CauseStatementBudget
		v.Detail = fmt.Sprintf("cancelled after the %s statement budget: the change does real rewrite work, "+
			"not an in-place catalog change, and needs a copy-and-swap rewrite that pg-sprite does not perform yet. "+
			"If it adds a constraint, ADD CONSTRAINT ... NOT VALID followed by VALIDATE CONSTRAINT avoids the long lock",
			budgetErr.Budget)
	default:
		v.Detail = budgetErr.Error()
	}
	return v
}

// qualified renders the statement's target table for the verdict, empty when
// the statement has none.
func qualified(st statement.Statement) string {
	if st.Table() == "" {
		return ""
	}
	if st.Schema() == "" {
		return st.Table()
	}
	return st.Schema() + "." + st.Table()
}
