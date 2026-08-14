package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// run is the migrate flow: gate the statement type, classify and route it
// exactly as dry-run would, execute the routed SQL — the planner's safer
// native sequence by default when the submitted form blocks — and end in
// exactly one verdict. Refusal verdicts are printed to out and returned as
// verdict.ErrRefused so the entry point maps them to the refusal exit code;
// an execution failure prints a failed verdict — the stable executor code
// plus the committed prefix — and still returns the operational error.
// --dry-run diverts to the classify-and-route plan instead.
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

	st, err = resolveTarget(ctx, pool, st, logger)
	if err != nil {
		return err
	}
	if c.Force != "" {
		if err := c.checkForceAck(st); err != nil {
			return err
		}
	}

	facts, err := dryRunFacts(ctx, pool, st)
	if err != nil {
		return err
	}
	canonical, err := statement.Canonical(st.SQL())
	if err != nil {
		return err
	}
	classified, err := planner.Classify(canonical, facts)
	if err != nil {
		return err
	}
	routed := router.Route([]planner.Plan{classified})
	rs := routed.Statements[0]
	logger.Debug("statement routed",
		"route", string(classified.Route), "disposition", string(rs.Disposition))

	switch rs.Disposition {
	case router.DispositionExecute:
		execSQL := rs.ExecSQL
		substituted := len(execSQL) != 1 || execSQL[0] != rs.Statement
		forced := substituted && c.Force != ""
		if forced {
			// The acknowledged override: run the submitted form as a
			// blind bounded attempt instead of the safer sequence.
			execSQL, substituted = []string{canonical}, false
			c.auditForce(st, rs)
		}
		return c.execute(ctx, out, pool, st, execSQL, rs.Plan, substituted, forced, logger)
	case router.DispositionRewriteRequired:
		if c.Force == "" {
			return c.emit(out, rewriteRequiredVerdict(st))
		}
		c.auditForce(st, rs)
		return c.execute(ctx, out, pool, st, []string{canonical}, rs.Plan, false, true, logger)
	case router.DispositionUnavailable:
		if c.Force == "" {
			return c.emit(out, backendUnavailableVerdict(st, rs))
		}
		c.auditForce(st, rs)
		return c.execute(ctx, out, pool, st, []string{canonical}, rs.Plan, false, true, logger)
	case router.DispositionRefuse:
		// A planner refusal means no known safe path — there is nothing
		// bounded to acknowledge, so --force does not apply.
		return c.emit(out, routeRefusalVerdict(st, rs))
	default:
		// A disposition this build does not know is a router/CLI version
		// skew; refuse to act rather than guess.
		return fmt.Errorf("unknown disposition %q", rs.Disposition)
	}
}

// resolveTarget qualifies an unqualified statement against the session's
// search_path exactly once and re-emits it in schema-qualified form, so
// every later stage — facts, classification, the planner's safer sequences,
// preflight, and the executor — names the same relation regardless of any
// session's search_path. The executor's own unqualified-table refusals stay:
// this is the CLI resolving its user's intent, not the executor trusting a
// name. Statements already qualified (or without a table target) pass
// through unchanged.
func resolveTarget(ctx context.Context, pool *pgxpool.Pool, st statement.Statement,
	logger *slog.Logger) (statement.Statement, error) {
	if st.Schema() != "" || st.Table() == "" {
		return st, nil
	}
	// Re-emitting goes through the deparser, which drops comments; refuse
	// commented input instead of silently discarding content.
	if err := statement.CheckNoComments(st.SQL()); err != nil {
		return statement.Statement{}, err
	}
	const q = `
		SELECT n.nspname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass(quote_ident($1))`
	var schema string
	err := pool.QueryRow(ctx, q, st.Table()).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return statement.Statement{}, fmt.Errorf("%w: %s is not visible on the session search_path",
			preflight.ErrTableNotFound, st.Table())
	}
	if err != nil {
		return statement.Statement{}, fmt.Errorf("resolve %s against search_path: %w", st.Table(), err)
	}
	sql, err := statement.Qualify(st.SQL(), schema)
	if err != nil {
		return statement.Statement{}, fmt.Errorf("qualify %s as %s.%s: %w", st.Table(), schema, st.Table(), err)
	}
	logger.Debug("unqualified table resolved", "table", st.Table(), "schema", schema)
	return statement.ParseOne(sql)
}

// checkForceAck validates the --force acknowledgement: it must name the
// resolved schema-qualified target table exactly, proving the operator
// names the relation whose lock they are accepting. A mismatch is a usage
// error — nothing has executed.
func (c *MigrateCmd) checkForceAck(st statement.Statement) error {
	if c.Force == qualified(st) {
		return nil
	}
	return fmt.Errorf("--force must acknowledge the resolved target table %q, got %q; nothing was executed",
		qualified(st), c.Force)
}

// auditForce records the override decision before anything executes: the
// operator chose the submitted form over the engine's routing. The record
// is warn-level and unconditional — an audit trail must not depend on
// --debug — and the verdict's Forced field is its machine-readable twin.
func (c *MigrateCmd) auditForce(st statement.Statement, rs router.Statement) {
	c.audit().Warn("forced execution of submitted form",
		"table", qualified(st),
		"kind", st.Kind().String(),
		"disposition", string(rs.Disposition))
}

// execute runs execSQL through the sequence executor: the planner's safer
// sequence when one was substituted, otherwise the submitted form. A forced
// run bypasses the sequence executor's shape admission — the acknowledged
// override runs the submitted form as one blind bounded attempt, whatever
// its kind — so it goes through the optimistic executor directly, under the
// same brief budgets. Blind attempts of the submitted form — including
// forced ones — are size-guarded; substituted sequences and planner-proven
// online idioms are not — long work on large tables is their purpose, and
// every brief step is still budget-bounded. Before anything runs, the
// connected role is checked at the tier the routed steps actually need
// (engine-role contract), so a role that would die mid-change is refused
// with the exact provisioning statement instead.
func (c *MigrateCmd) execute(ctx context.Context, out io.Writer, pool *pgxpool.Pool,
	st statement.Statement, execSQL []string, plan planner.Plan,
	substituted, forced bool, logger *slog.Logger) error {
	tier, err := preflight.RequiredTier(execSQL)
	if err != nil {
		return err
	}
	priv, err := preflight.CheckPrivileges(ctx, pool, st.Schema(), st.Table(),
		preflight.Requirement{Tier: tier})
	var privErr *preflight.PrivilegeError
	if errors.As(err, &privErr) {
		return c.emit(out, privilegeVerdict(st, privErr, forced))
	}
	if err != nil {
		return err
	}
	logger.Debug("privilege preflight passed",
		"role", priv.Role(), "owner", priv.Owner(), "tier", tier.String())

	limit := int64(c.MaxTableSize)
	if !sizeGuardApplies(plan, substituted) {
		limit = preflight.NoSizeLimit
	}
	pt, err := preflight.CheckTable(ctx, pool, st.Schema(), st.Table(), limit)
	var sizeErr *preflight.SizeError
	if errors.As(err, &sizeErr) {
		return c.emit(out, sizeGuardVerdict(st, sizeErr, forced))
	}
	if err != nil {
		return err
	}
	if err := preflight.CheckPartitionSupport(pt, execSQL); err != nil {
		var partitionErr *preflight.UnsupportedPartitionedParentError
		if errors.As(err, &partitionErr) {
			return c.emit(out, partitionedParentVerdict(st, partitionErr, forced))
		}
		return err
	}
	logger.Debug("preflight passed",
		"table", qualified(st), "total_bytes", pt.TotalBytes(), "limit_bytes", limit)
	if substituted {
		logger.Debug("substituting safer native sequence",
			"table", qualified(st), "steps", len(execSQL))
	}

	budget := executor.SequenceBudget{
		Brief:      executor.Budget{LockTimeout: c.LockTimeout, StatementTimeout: c.StatementTimeout},
		Concurrent: executor.ConcurrentBudget{Overall: c.IndexBuildTimeout},
		Validate:   executor.ValidateBudget{LockTimeout: c.LockTimeout, Overall: c.ValidateTimeout},
	}
	retry := c.retryPolicy()
	start := time.Now()
	var rep executor.SequenceReport
	if forced {
		err = executor.ExecuteNative(ctx, pool, pt, st, budget.Brief, retry)
	} else {
		rep, err = executor.RunSequence(ctx, pool, pt, execSQL, budget, retry)
	}
	elapsed := time.Since(start)
	if v, refused := execRefusal(st, err, substituted, forced, onlineIdiomPlan(plan)); refused {
		logger.Debug("execution refused",
			"reason", string(v.Reason), "cause", string(v.Cause), "attempts", v.Attempts, "elapsed", elapsed)
		return c.emit(out, v)
	}
	if err != nil {
		// Everything else is an operational failure, not a refusal: the
		// typed *SequenceStepError names the failed step and the committed
		// prefix that remains, and an *InvalidIndexError carries the
		// operator recovery guidance. The failed verdict is the error's
		// machine-readable twin on stdout — the stable executor code, the
		// failed step, and the committed prefix — while the error itself
		// still returns, so the process exits 1, not the refusal code.
		v := failureVerdict(st, err, rep, forced)
		logger.Debug("execution failed",
			"code", v.Code, "failed_step", v.FailedStep, "committed_steps", len(v.ExecutedSQL), "elapsed", elapsed)
		if emitErr := c.emit(out, v); emitErr != nil {
			return emitErr
		}
		return fmt.Errorf("run schema change on %s: %w", qualified(st), err)
	}
	logger.Debug("schema change committed",
		"table", qualified(st), "steps", len(execSQL), "elapsed", elapsed)

	v := verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail: fmt.Sprintf("committed within budgets (lock %s, statement %s): the change was effectively instant",
			c.LockTimeout, c.StatementTimeout),
	}
	if substituted {
		v.ExecutedSQL = execSQL
		v.Detail = fmt.Sprintf("the submitted form blocks; pg-sprite ran the safer native sequence instead — all %d steps committed",
			len(execSQL))
	}
	if forced {
		v.Detail = fmt.Sprintf("forced: the submitted form ran as-is under budgets (lock %s, statement %s), overriding the engine's routing",
			c.LockTimeout, c.StatementTimeout)
	}
	return c.emit(out, v)
}

// failureVerdict maps an operational execution failure to its failed
// verdict: the executor's stable outcome code, and for a mid-sequence
// failure the failed step and the committed prefix whose state remains.
// It is the machine-readable twin of the returned error — automation
// branches on Code and ExecutedSQL instead of parsing stderr prose. A
// single bounded attempt (the submitted form, forced or not) rolls back on
// failure, so it carries no step and an empty committed prefix.
func failureVerdict(st statement.Statement, err error,
	rep executor.SequenceReport, forced bool) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.OutcomeCode(err)),
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail:    "execution failed; nothing committed — a started bounded attempt rolls back",
	}
	var stepErr *executor.SequenceStepError
	if !errors.As(err, &stepErr) {
		return v
	}
	v.FailedStep = stepErr.Step
	v.FailedStepSQL = stepErr.SQL
	for _, s := range rep.Steps {
		v.ExecutedSQL = append(v.ExecutedSQL, s.SQL)
	}
	if len(v.ExecutedSQL) > 0 {
		v.Detail = fmt.Sprintf("sequence step %d of %d failed; the %d committed steps' state remains — the planner sequence's partial-failure contract says how a retry resumes",
			stepErr.Step, stepErr.Total, len(v.ExecutedSQL))
	} else {
		v.Detail = fmt.Sprintf("sequence step %d of %d failed; no earlier steps had committed — Code names the outcome and any state the failed step itself left",
			stepErr.Step, stepErr.Total)
	}
	return v
}

// execRefusal maps an execution failure to its refusal verdict, when the
// failure belongs to the refusal contract rather than the operational-error
// exit: a static admission refusal (decided before anything executed), or a
// budget cancellation of a non-substituted attempt. An *InvalidIndexError
// is never a refusal, even when a budget cancellation is buried inside it —
// invalid-index debris is the one outcome that needs an operator, and its
// typed error carries the recovery guidance a budget verdict would conceal.
func execRefusal(st statement.Statement, err error,
	substituted, forced, online bool) (verdict.Verdict, bool) {
	if err == nil {
		return verdict.Verdict{}, false
	}
	if isAdmissionRefusal(err) {
		return admissionRefusalVerdict(st, err, forced), true
	}
	var invalidErr *executor.InvalidIndexError
	if errors.As(err, &invalidErr) {
		return verdict.Verdict{}, false
	}
	var budgetErr *executor.BudgetError
	if !substituted && errors.As(err, &budgetErr) {
		// The blind attempt of the submitted form exceeded a budget and was
		// cancelled without committing — the Phase 1 refusal contract. A
		// forced attempt is bounded by the same budgets: --force overrides
		// routing, never the executor's protections.
		return budgetVerdict(st, budgetErr, forced, online), true
	}
	return verdict.Verdict{}, false
}

// isAdmissionRefusal reports whether err is one of the executor's static
// admission refusals: decided from the statement's shape before anything
// executes, so it maps to a refusal verdict, not an operational error. A
// *SequenceStepError wrapper means execution started, which is never an
// admission refusal.
func isAdmissionRefusal(err error) bool {
	var stepErr *executor.SequenceStepError
	if errors.As(err, &stepErr) {
		return false
	}
	return errors.Is(err, executor.ErrUnsupportedSequenceStep) ||
		errors.Is(err, executor.ErrUnnamedIndex) ||
		errors.Is(err, executor.ErrIfNotExistsUnsupported)
}

// onlineIdiomPlan reports whether the plan proved every operation an online
// idiom (CONCURRENTLY, NOT VALID, VALIDATE): the submitted form already is
// the safe pattern, and running long on a large table is its purpose.
func onlineIdiomPlan(p planner.Plan) bool {
	for _, d := range p.Decisions {
		if d.Reason != planner.ReasonOnlineIdiom {
			return false
		}
	}
	return true
}

// sizeGuardApplies reports whether the size guard protects this run. It
// guards exactly the blind attempt of the submitted form: when the engine
// substituted the planner's safer sequence, or when the plan proved every
// operation an online idiom, long work on a large table is the pattern's
// purpose and the guard would refuse the very tables the pattern serves.
func sizeGuardApplies(p planner.Plan, substituted bool) bool {
	return !substituted && !onlineIdiomPlan(p)
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

// gateVerdict is the statement-type gate: ALTER TABLE and CREATE INDEX
// proceed to classification (a blocking CREATE INDEX is substituted with
// its concurrent build, a submitted concurrent build is driven directly);
// the index-maintenance forms the executor cannot drive yet are pointed at
// their concurrent idiom, everything else is unsupported. Refused
// statements are never executed.
func gateVerdict(st statement.Statement) (verdict.Verdict, bool) {
	v := verdict.Verdict{Outcome: verdict.OutcomeRefused, Statement: st.SQL()}
	switch st.Kind() {
	case statement.KindAlterTable, statement.KindCreateIndex:
		return verdict.Verdict{}, false
	case statement.KindDropIndex, statement.KindReindex:
		v.Reason = verdict.ReasonIndexStatement
		v.Detail, v.SaferIdiom = indexAdvice(st)
	case statement.KindCreateTable:
		v.Reason = verdict.ReasonUnsupportedStatement
		v.Detail = "migrate changes an existing table; to converge a table onto a desired-state CREATE TABLE, use the declarative front-end"
		v.SaferIdiom = "pg-sprite diff --desired schema.sql"
	case statement.KindOther:
		v.Reason = verdict.ReasonUnsupportedStatement
		v.Detail = "only ALTER TABLE and CREATE INDEX statements are supported by the imperative front door"
	}
	return v, true
}

// indexAdvice explains an index-statement refusal for the maintenance forms
// the executor does not drive (DROP INDEX, REINDEX). The already-concurrent
// forms carry no safer idiom: suggesting the statement the user submitted
// would confuse a human once and send a resubmitting automation into a loop.
func indexAdvice(st statement.Statement) (detail, saferIdiom string) {
	if st.Concurrent() {
		return "this is already the safe concurrent idiom; pg-sprite does not drive this maintenance form yet — run it directly against the database", ""
	}
	switch st.Kind() {
	case statement.KindDropIndex:
		return "a plain DROP INDEX takes ACCESS EXCLUSIVE on the table; the concurrent drop does not", "DROP INDEX CONCURRENTLY"
	case statement.KindReindex:
		return "a plain REINDEX blocks writes; the concurrent rebuild does not", "REINDEX ... CONCURRENTLY"
	default:
		return "", ""
	}
}

// privilegeVerdict is the refusal for a connected role that lacks the access
// the routed change needs. The error already names the failed catalog check
// and the exact provisioning statement, so it is the detail verbatim. A
// refused forced attempt still records the override: the operator asked for
// the submitted form and the role could not run it.
func privilegeVerdict(st statement.Statement, privErr *preflight.PrivilegeError, forced bool) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonInsufficientPrivileges,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail:    privErr.Error(),
	}
}

// partitionedParentVerdict refuses routed index-building steps on a
// partitioned parent before the sequence executor runs anything.
func partitionedParentVerdict(st statement.Statement, partitionErr *preflight.UnsupportedPartitionedParentError,
	forced bool) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonUnsupportedPartitionedParent,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail:    partitionErr.Error(),
	}
}

// rewriteRequiredVerdict is the refusal for a statement whose submitted
// form blocks but for which the planner could not construct the safer
// native sequence — a multi-operation statement, or a pattern it cannot
// build. Running the submitted form would falsify the plan's own reason.
func rewriteRequiredVerdict(st statement.Statement) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonRewriteRequired,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: "the submitted form blocks and must run as a safer native sequence, but pg-sprite could not " +
			"construct one for this statement; submit each operation as its own single-operation statement " +
			"so the engine can build its safer form (run with --dry-run to see each operation's classification)",
	}
}

// backendUnavailableVerdict is the refusal for a change that routes to an
// execution strategy this build does not implement.
func backendUnavailableVerdict(st statement.Statement, rs router.Statement) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonBackendUnavailable,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: fmt.Sprintf("the change requires the %s strategy, which this build does not implement yet: "+
			"PostgreSQL would rewrite the table under ACCESS EXCLUSIVE for the whole operation", rs.Backend),
	}
}

// routeRefusalVerdict is the refusal for a statement the planner refused:
// it carries the refused operations by name so the operator knows which
// part of the statement the engine does not know a safe path for.
func routeRefusalVerdict(st statement.Statement, rs router.Statement) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonUnsupportedStatement,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    "the planner knows no safe path for this statement",
	}
	for _, d := range rs.Decisions {
		if d.Route == planner.RouteRefuse {
			v.Detail = fmt.Sprintf("the planner knows no safe path for %s", d.Operation)
			break
		}
	}
	return v
}

// sizeGuardVerdict is the refusal for tables above the size threshold, where
// even a budget-bounded attempt would visibly stall the table. A refused
// forced attempt still records the override: the operator asked for the
// submitted form and the guard said no.
func sizeGuardVerdict(st statement.Statement, sizeErr *preflight.SizeError, forced bool) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonTableTooLarge,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail: fmt.Sprintf("table is %d bytes on disk (heap, indexes, and TOAST), above the %d-byte "+
			"--max-table-size threshold. pg-sprite cannot yet prove this change is instant on a table this "+
			"size; if it requires a rewrite, a cancelled attempt is not a free probe — it would hold "+
			"ACCESS EXCLUSIVE doing rewrite work for the whole budget",
			sizeErr.TotalBytes, sizeErr.LimitBytes),
	}
}

// admissionRefusalVerdict is the refusal for a statement the gate admits but
// the executor's static admission refuses before anything executes: an
// unnamed index build, IF NOT EXISTS on a concurrent build, or a substituted
// step shape the sequence executor does not drive yet (DETACH PARTITION
// CONCURRENTLY). The typed error carries the explanation.
func admissionRefusalVerdict(st statement.Statement, err error, forced bool) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonUnsupportedStatement,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail:    fmt.Sprintf("the engine cannot run this statement safely: %v; nothing was executed", err),
	}
}

// budgetVerdict is the refusal for an attempt that exceeded its lock or
// statement budget and was cancelled without executing. online tailors the
// statement-budget advice: a submitted form the plan proved an online idiom
// (a concurrent build, a lone VALIDATE) needs a larger budget, not a
// different strategy, while a blind attempt that ran past its budget is
// doing rewrite work. A refused forced attempt still records the override.
func budgetVerdict(st statement.Statement, budgetErr *executor.BudgetError, forced, online bool) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonBudgetExceeded,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
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
			"contended right now; nothing was executed", budgetErr.Budget)
	case executor.CauseStatement:
		v.Cause = verdict.CauseStatementBudget
		if online {
			v.Detail = fmt.Sprintf("cancelled after the %s budget: the statement already is the safe online "+
				"idiom — the work needs more time, not a different strategy; retry with a larger budget for "+
				"this step class", budgetErr.Budget)
		} else {
			v.Detail = fmt.Sprintf("cancelled after the %s statement budget: the change does real rewrite work, "+
				"not an in-place catalog change, and needs a copy-and-swap rewrite that pg-sprite does not perform yet. "+
				"If it adds a constraint, ADD CONSTRAINT ... NOT VALID followed by VALIDATE CONSTRAINT avoids the long lock",
				budgetErr.Budget)
		}
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
