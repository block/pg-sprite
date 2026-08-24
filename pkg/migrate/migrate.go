// Package migrate is the imperative front door as a library: one parsed
// statement in — gate, resolve, introspect, classify, route, execute — and
// exactly one verdict out. The CLI's migrate command and orchestrators
// embedding pg-sprite share this one pipeline, so a verdict means the same
// thing no matter which caller produced it.
//
// [RunDesired] is the declarative execution loop on the same pipeline: one
// parsed desired-state schema in, the convergence plan derived through
// diffplan.Plan, and every planned statement executed back through [Run] —
// per-statement verdicts, committed-prefix semantics, stop at the first
// refusal or failure. Both entry points share [Options], the executors,
// and the verdict contract; the desired loop adds only plan admission and
// sequencing.
//
// Callers own the boundary concerns: parse the statement through
// [statement.ParseOne] (or the desired schema through
// [statement.ParseDesired]; a parse failure surfaces at the caller) and
// build the connection through [dbconn.NewPool]. [Gate] is exported so a
// caller can refuse an unsupported statement kind before dialing; [Run]
// re-checks it, so a caller that skips the early gate still cannot execute
// a gated kind.
//
// [Run] takes the concrete [pgxpool.Pool] that [dbconn.NewPool] returns —
// a deliberate concrete dependency, not an oversight: the execution paths
// need the full pool surface (dedicated sessions for concurrent builds,
// per-step transactions), a narrower interface would admit handles those
// paths cannot use, and it is the same handle the declarative front door
// (diffplan.Plan) takes, so the two front doors embed identically.
//
// Before a v1 module tag the Go API carries no compatibility promise: the
// JSON [verdict.Verdict] is the stability boundary, the Go API follows at
// v1 (see docs/architecture.md).
package migrate

import (
	"context"
	"errors"
	"fmt"
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

// Options carries the execution policy for one [Run]: the safety budgets,
// the retry policy, the size guard, and the operator's force
// acknowledgement. The zero value is not a runnable policy: callers set the
// budgets and the size guard deliberately — [DefaultOptions] is the
// sanctioned starting point (the same policy the CLI's flag defaults
// wire), an embedding orchestrator tunes from there.
type Options struct {
	// Force is the typed acknowledgement to run the submitted form as-is,
	// overriding a safer-sequence substitution or a rewrite-required /
	// backend-unavailable refusal. It must name the resolved
	// schema-qualified target table exactly; empty means the engine's
	// routing decides. Planner refusals (no known safe path) and gated
	// statement kinds cannot be forced.
	Force string

	// MaxTableSizeBytes is the threshold above which a blind bounded
	// attempt of the submitted form is refused, measured as the table's
	// full on-disk footprint: heap, indexes, and TOAST, all partitions.
	// Substituted safer sequences and planner-proven online idioms are not
	// size-guarded — long work on large tables is their purpose.
	MaxTableSizeBytes int64

	// Budget bounds every executed step: brief steps under lock and
	// statement timeouts, concurrent index builds and constraint
	// validation under their overall bounds.
	Budget executor.SequenceBudget

	// Retry is the bounded retry policy when native DDL exceeds its lock
	// budget. The zero value uses [executor.DefaultRetryPolicy]; a
	// partially configured policy is rejected by the executor.
	Retry executor.RetryPolicy

	// Logger receives decision diagnostics (routing, preflight, execution
	// transitions); nil discards them.
	Logger *slog.Logger

	// Audit receives the force-override audit record; nil discards it.
	// The verdict's Forced field is the machine-readable record and is
	// always set, so the run's outcome never depends on this logger; the
	// CLI wires an always-on stderr handler here so an operator's
	// deliberate safety override is visible even without diagnostics.
	Audit *slog.Logger
}

// DefaultOptions is the sanctioned starting point for an embedding caller:
// the same budgets, size guard, and retry policy the CLI's flag defaults
// wire. These are defaults to tune, not a recommendation — a large table
// needs a more generous concurrent-build and validate bound, a hot table a
// tighter lock budget. Force, Logger, and Audit stay zero: overriding
// safety and receiving diagnostics are always deliberate choices.
func DefaultOptions() Options {
	return Options{
		MaxTableSizeBytes: 1 << 30,
		Budget: executor.SequenceBudget{
			Brief:      executor.Budget{LockTimeout: 3 * time.Second, StatementTimeout: 30 * time.Second},
			Concurrent: executor.ConcurrentBudget{Overall: 30 * time.Minute},
			Validate:   executor.ValidateBudget{LockTimeout: 3 * time.Second, Overall: 30 * time.Minute},
		},
		Retry: executor.DefaultRetryPolicy(),
	}
}

// logger returns the diagnostics logger, discarding when the caller wired
// none.
func (o Options) logger() *slog.Logger {
	if o.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return o.Logger
}

// audit returns the audit logger, discarding when the caller wired none —
// the same split Logger has, so the library never writes to the host
// process's stderr behind an embedder's logging stack.
func (o Options) audit() *slog.Logger {
	if o.Audit == nil {
		return slog.New(slog.DiscardHandler)
	}
	return o.Audit
}

// validate rejects an unrunnable policy at the front door, before any
// database work, so the documented "the zero value is not a runnable
// policy" contract holds on every path — not only the paths that happen to
// reach the size guard. Budgets and the retry policy carry their own
// validation in the executor.
func (o Options) validate() error {
	if o.MaxTableSizeBytes <= 0 {
		return fmt.Errorf("migrate: Options.MaxTableSizeBytes must be positive, got %d; DefaultOptions is the sanctioned starting point", o.MaxTableSizeBytes)
	}
	return nil
}

// retry returns the retry policy, preserving the safe defaults for a
// zero-valued policy while leaving partially configured or invalid
// policies for the executor to reject.
func (o Options) retry() executor.RetryPolicy {
	if o.Retry == (executor.RetryPolicy{}) {
		return executor.DefaultRetryPolicy()
	}
	return o.Retry
}

// Run drives one schema change end to end: gate the statement type,
// resolve the target, classify and route the statement exactly as a dry
// run would, execute the routed SQL — the planner's safer native sequence
// by default when the submitted form blocks — and end in exactly one
// verdict.
//
// The verdict-and-error contract has three shapes. A refusal returns the
// refusal verdict and a nil error; the caller maps it to its refusal exit
// path. An execution failure returns the failed verdict — the stable
// executor code plus the committed prefix — together with the operational
// error; the verdict is the error's machine-readable twin. An error with a
// zero verdict means the pipeline stopped before reaching a verdict (a
// resolution, introspection, or acknowledgement error) and nothing was
// executed.
//
// Run does not close the pool; one pool serves any number of calls.
func Run(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, opts Options) (verdict.Verdict, error) {
	if err := opts.validate(); err != nil {
		return verdict.Verdict{}, err
	}
	logger := opts.logger()
	if v, refused := Gate(st); refused {
		return v, nil
	}
	st, err := resolveTarget(ctx, pool, st, logger)
	if err != nil {
		return verdict.Verdict{}, err
	}
	if opts.Force != "" {
		if err := checkForceAck(st, opts.Force); err != nil {
			return verdict.Verdict{}, err
		}
	}

	facts, err := LiveFacts(ctx, pool, st)
	if err != nil {
		return verdict.Verdict{}, err
	}
	canonical, err := statement.Canonical(st.SQL())
	if err != nil {
		return verdict.Verdict{}, err
	}
	classified, err := planner.Classify(canonical, facts.Classifier)
	if err != nil {
		return verdict.Verdict{}, err
	}
	routed := router.Route([]planner.Plan{classified})
	rs := routed.Statements[0]
	logger.Debug("statement routed",
		"route", string(classified.Route), "disposition", string(rs.Disposition))

	switch rs.Disposition {
	case router.DispositionExecute:
		execSQL := rs.ExecSQL
		substituted := len(execSQL) != 1 || execSQL[0] != rs.Statement
		forced := substituted && opts.Force != ""
		if forced {
			// The acknowledged override: run the submitted form as a
			// blind bounded attempt instead of the safer sequence.
			execSQL, substituted = []string{canonical}, false
			auditForce(opts.audit(), st, rs)
		}
		return execute(ctx, pool, st, execSQL, rs.Plan, substituted, forced, opts, logger)
	case router.DispositionRewriteRequired:
		if opts.Force == "" {
			return rewriteRequiredVerdict(st), nil
		}
		auditForce(opts.audit(), st, rs)
		return execute(ctx, pool, st, []string{canonical}, rs.Plan, false, true, opts, logger)
	case router.DispositionUnavailable:
		if opts.Force == "" {
			return backendUnavailableVerdict(st, rs), nil
		}
		auditForce(opts.audit(), st, rs)
		return execute(ctx, pool, st, []string{canonical}, rs.Plan, false, true, opts, logger)
	case router.DispositionRefuse:
		// A planner refusal means no known safe path — there is nothing
		// bounded to acknowledge, so the force acknowledgement does not
		// apply.
		return routeRefusalVerdict(st, rs), nil
	default:
		// A disposition this build does not know is a router version
		// skew; refuse to act rather than guess.
		return verdict.Verdict{}, fmt.Errorf("unknown disposition %q", rs.Disposition)
	}
}

// resolveTarget qualifies an unqualified statement against the session's
// search_path exactly once and re-emits it in schema-qualified form, so
// every later stage — facts, classification, the planner's safer sequences,
// preflight, and the executor — names the same relation regardless of any
// session's search_path. The executor's own unqualified-table refusals
// stay: this is the front door resolving its caller's intent, not the
// executor trusting a name. Statements already qualified (or without a
// table target) pass through unchanged.
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

// checkForceAck validates the force acknowledgement: it must name the
// resolved schema-qualified target table exactly, proving the operator
// names the relation whose lock they are accepting. A mismatch is a usage
// error — nothing has executed.
func checkForceAck(st statement.Statement, ack string) error {
	if ack == qualified(st) {
		return nil
	}
	return fmt.Errorf("the force acknowledgement must name the resolved target table %q, got %q; nothing was executed",
		qualified(st), ack)
}

// auditForce records the override decision before anything executes: the
// operator chose the submitted form over the engine's routing. The record
// is warn-level and unconditional — an audit trail must not depend on
// diagnostics being enabled — and the verdict's Forced field is its
// machine-readable twin.
func auditForce(audit *slog.Logger, st statement.Statement, rs router.Statement) {
	audit.Warn("forced execution of submitted form",
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
func execute(ctx context.Context, pool *pgxpool.Pool, st statement.Statement,
	execSQL []string, plan planner.Plan, substituted, forced bool,
	opts Options, logger *slog.Logger) (verdict.Verdict, error) {
	limit := opts.MaxTableSizeBytes
	if !sizeGuardApplies(plan, substituted) {
		limit = preflight.NoSizeLimit
	}
	pt, err := preflight.CheckTable(ctx, pool, st.Schema(), st.Table(), limit)
	var sizeErr *preflight.SizeError
	if errors.As(err, &sizeErr) {
		return sizeGuardVerdict(st, sizeErr, forced), nil
	}
	if err != nil {
		return verdict.Verdict{}, err
	}
	serverMajor, err := dbconn.ServerMajor(ctx, pool)
	if err != nil {
		return verdict.Verdict{}, err
	}
	if err := preflight.CheckPartitionSupport(pt, serverMajor, execSQL); err != nil {
		var partitionErr *preflight.UnsupportedPartitionedParentError
		if errors.As(err, &partitionErr) {
			return partitionedParentVerdict(st, partitionErr, forced), nil
		}
		return verdict.Verdict{}, err
	}
	tier, err := preflight.RequiredTier(execSQL)
	if err != nil {
		return verdict.Verdict{}, err
	}
	priv, err := preflight.CheckPrivileges(ctx, pool, st.Schema(), st.Table(),
		preflight.Requirement{Tier: tier})
	var privErr *preflight.PrivilegeError
	if errors.As(err, &privErr) {
		return privilegeVerdict(st, privErr, forced), nil
	}
	if err != nil {
		return verdict.Verdict{}, err
	}
	logger.Debug("privilege preflight passed",
		"role", priv.Role(), "owner", priv.Owner(), "tier", tier.String())
	logger.Debug("preflight passed",
		"table", qualified(st), "total_bytes", pt.TotalBytes(), "limit_bytes", limit)
	if substituted {
		logger.Debug("substituting safer native sequence",
			"table", qualified(st), "steps", len(execSQL))
	}

	retry := opts.retry()
	start := time.Now()
	var rep executor.SequenceReport
	if forced {
		err = executor.ExecuteNative(ctx, pool, pt, st, opts.Budget.Brief, retry)
	} else {
		rep, err = executor.RunSequence(ctx, pool, pt, execSQL, opts.Budget, retry)
	}
	elapsed := time.Since(start)
	if v, refused := execRefusal(st, err, substituted, forced, onlineIdiomPlan(plan)); refused {
		logger.Debug("execution refused",
			"reason", string(v.Reason), "cause", string(v.Cause), "attempts", v.Attempts, "elapsed", elapsed)
		return v, nil
	}
	if err != nil {
		// Everything else is an operational failure, not a refusal: the
		// typed *SequenceStepError names the failed step and the committed
		// prefix that remains, and an *InvalidIndexError carries the
		// operator recovery guidance. The failed verdict is the error's
		// machine-readable twin — the stable executor code, the failed
		// step, and the committed prefix — while the error itself still
		// returns, so the caller's operational-error exit applies, not its
		// refusal exit.
		v := failureVerdict(st, err, rep, forced)
		logger.Debug("execution failed",
			"code", v.Code, "failed_step", v.FailedStep, "committed_steps", len(v.ExecutedSQL), "elapsed", elapsed)
		return v, fmt.Errorf("run schema change on %s: %w", qualified(st), err)
	}
	logger.Debug("schema change committed",
		"table", qualified(st), "steps", len(execSQL), "elapsed", elapsed)

	v := verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Table:     qualified(st),
		Forced:    forced,
		Detail: fmt.Sprintf("committed within budgets (lock %s, statement %s): the change was effectively instant",
			opts.Budget.Brief.LockTimeout, opts.Budget.Brief.StatementTimeout),
	}
	if substituted {
		v.ExecutedSQL = execSQL
		v.Detail = fmt.Sprintf("the submitted form blocks; pg-sprite ran the safer native sequence instead — all %d steps committed",
			len(execSQL))
	}
	if forced {
		v.Detail = fmt.Sprintf("forced: the submitted form ran as-is under budgets (lock %s, statement %s), overriding the engine's routing",
			opts.Budget.Brief.LockTimeout, opts.Budget.Brief.StatementTimeout)
	}
	return v, nil
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
