package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// This file is desired-state execution's one blocking statement: the drop
// of a partitioned table's index, which PostgreSQL cannot drop
// concurrently. The plan keeps the statement refused and discloses the
// plain DROP INDEX with the execution blocking-index-drop; a run lifts that
// one refusal only when the request pins the reviewed plan, names the table
// in the destructive acknowledgement, and names it again in
// DesiredRequest.AcceptBlocking.

// ErrAcceptBlockingNeedsReviewedDrop is returned by [RunDesired] when the
// request names a table in AcceptBlocking without pinning a plan
// fingerprint and naming the table in AllowDestructive: the blocking drop it
// accepts is a destructive statement of a reviewed plan, and both of those
// bind it. No database was used and nothing has executed.
var ErrAcceptBlockingNeedsReviewedDrop = errors.New(
	"the accept-blocking acknowledgement on a desired-state run is valid only with the fingerprint of the reviewed plan " +
		"and the destructive acknowledgement; nothing was executed")

// checkDesiredAcceptBlocking is the usage check a desired-state request's
// accept-blocking acknowledgement passes before any database work: it comes
// with the pin and the destructive acknowledgement, and the run's brief
// budget can bound the blocking drop, so a budget the drop would refuse
// stops the run before any planned statement commits.
func checkDesiredAcceptBlocking(req DesiredRequest, opts Options) error {
	if req.AcceptBlocking == "" {
		return nil
	}
	if req.ExpectedFingerprint == "" || req.AllowDestructive == "" {
		return fmt.Errorf("%w: it names %q", ErrAcceptBlockingNeedsReviewedDrop, req.AcceptBlocking)
	}
	return blockingDropBudget(opts).Validate()
}

// checkDesiredAcceptBlockingTable is the check a desired-state run makes on
// its accept-blocking acknowledgement once the plan is derived and before
// anything runs: it must name the table report converges, whether or not
// the plan drops a partitioned index, so an acknowledgement left on a script
// cannot reach another table.
func checkDesiredAcceptBlockingTable(accept string, report plan.Report) error {
	if accept == "" || accept == desiredTable(report) {
		return nil
	}
	return acceptBlockingMismatch(desiredTable(report), accept, statement.KindDropIndex)
}

// blockingDropBudget is the budget a planned blocking index drop runs
// under: the run's brief lock and statement timeouts.
func blockingDropBudget(opts Options) executor.BlockingBudget {
	return executor.BlockingBudget{
		LockTimeout:      opts.Budget.Brief.LockTimeout,
		StatementTimeout: opts.Budget.Brief.StatementTimeout,
	}
}

// acceptsBlockingDrop reports whether the request lifts the refusal of the
// planned statement ps: ps is a partitioned table's index drop carrying the
// blocking-index-drop execution, and the request pins the plan, names the
// table in its destructive acknowledgement, and accepts the table's blocking
// lock. Any other refused statement stays refused.
func acceptsBlockingDrop(req DesiredRequest, report plan.Report, ps plan.Statement) bool {
	return isPartitionedIndexDrop(ps) && ps.Execution == planner.ExecutionBlockingIndexDrop &&
		destructiveAcknowledged(req, report) && req.AcceptBlocking == desiredTable(report)
}

// partitionedIndexDropDetail is the admission refusal of a partitioned
// table's index drop the request does not accept: what the drop locks, and
// the acknowledgements that run it under bounded budgets.
func partitionedIndexDropDetail(report plan.Report, i int, ps plan.Statement) string {
	table := desiredTable(report)
	detail := fmt.Sprintf(
		"planned statement %d discards live structure (%s); PostgreSQL has no concurrent drop for an index of a "+
			"partitioned table, so its only drop is this plain DROP INDEX, which takes ACCESS EXCLUSIVE on %s, every "+
			"partition, and every partition's index for as long as it runs",
		i+1, ps.SQL, table)
	if ps.Execution != planner.ExecutionBlockingIndexDrop {
		return detail + "; this plan does not carry the drop as a blocking index drop, so desired-state execution does not run it — " +
			"drop it deliberately with this plain DROP INDEX through the imperative front door, acknowledging both the blocking lock and the destructive drop for the table"
	}
	return detail + fmt.Sprintf("; to run it under bounded budgets, review the plan, then rerun with ExpectedFingerprint %s, "+
		"AllowDestructive %s, and AcceptBlocking %s in the request (on the command line, --expect-fingerprint %[1]s "+
		"--allow-destructive %[2]s --accept-blocking %[2]s --statement-timeout DURATION, longer than --lock-timeout)",
		report.Fingerprint, table, table)
}

// dropPlannedPartitionedIndex runs the accepted blocking drop of a
// partitioned table's index, statement ps of an admitted plan at the given
// step: the plain DROP INDEX the plan discloses, in one engine-owned
// transaction under the run's brief budgets, after the executor reads the
// index again under the table's lock. A commit is the
// executed-without-online-safety verdict carrying the accepted refusal; an
// exhausted lock budget and a drop the executor will not run are refusals,
// with nothing dropped; every other failure is a failed verdict and an
// error.
func dropPlannedPartitionedIndex(ctx context.Context, pool *pgxpool.Pool, report plan.Report, ps plan.Statement,
	st statement.Statement, opts Options, acceptBlocking string, step int) (verdict.Verdict, error) {
	index, definition, err := plannedBlockingIndexDrop(report, ps, st)
	if err != nil {
		return verdict.Verdict{}, err
	}
	table := desiredTable(report)
	if acceptBlocking != table {
		return verdict.Verdict{}, fmt.Errorf("%w: planned statement %q is a blocking index drop, but the plan was admitted without accepting the lock on %s",
			executor.ErrInvariantViolation, ps.SQL, table)
	}
	proof, ok := partitionRefusal(preflight.PartitionCauseConcurrentIndexDrop)
	if !ok || !verdict.AcceptedBlockingEligible(proof) {
		return verdict.Verdict{}, fmt.Errorf("%w: the partitioned index drop's refusal is not eligible for the accepted blocking path",
			executor.ErrInvariantViolation)
	}
	budget := blockingDropBudget(opts)
	auditAllowDestructive(opts.audit(), st, table)
	auditAcceptBlocking(opts.audit(), st, table, proof, budget)

	opts.startStep(step, ps.SQL)
	rep, err := executor.ExecuteBlockingPartitionedIndexDrop(ctx, pool, report.Schema, report.Table, index, definition, budget)
	logger := opts.logger()
	base := verdict.Verdict{Statement: ps.SQL, Table: table, AllowedDestructive: true}
	if errors.Is(err, executor.ErrUnsupportedAcceptedBlocking) {
		v := admissionRefusalVerdict(st, err, verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), false)
		v.Table, v.AllowedDestructive = table, true
		return v, nil
	}
	if budgetErr, ok := errors.AsType[*executor.BudgetError](err); ok && budgetErr.Cause == executor.CauseLock {
		// INV: AB-2 — the lock was never granted, so nothing was dropped.
		v := budgetVerdict(st, budgetErr, false, false)
		v.Table, v.AllowedDestructive = table, true
		logger.Debug("planned blocking index drop refused", "table", table, "index", index, "reason", string(v.Reason))
		return v, nil
	}
	if err != nil {
		v := failureVerdict(st, err, executor.SequenceReport{}, false)
		v.Table, v.AllowedDestructive = table, true
		v.Detail = blockingIndexDropFailureDetail(err, table, index)
		v = WithServerError(v, err)
		logger.Debug("planned blocking index drop failed", "table", table, "index", index, "code", v.Code)
		return v, fmt.Errorf("drop index %s on %s under the accepted blocking lock: %w", index, table, err)
	}
	logger.Debug("planned blocking index drop committed", "table", table, "index", index, "elapsed", rep.Duration)
	v, err := base.WithAcceptedBlocking(proof, budget.LockTimeout, budget.StatementTimeout)
	if err != nil {
		return verdict.Verdict{}, fmt.Errorf("%w: %w", executor.ErrInvariantViolation, err)
	}
	v.ExecutedSQL = slices.Clone(ps.ExecSQL)
	v.Detail = fmt.Sprintf("the operator accepted the blocking drop: PostgreSQL has no concurrent drop for an index of a partitioned table, "+
		"so the plain DROP INDEX ran in %s under budgets (lock %s, statement %s) with no online-safety guarantee; %s, every partition of it, "+
		"and every partition's index were locked for the statement's duration, and the drop removed %s",
		rep.Duration.Round(time.Millisecond), budget.LockTimeout, budget.StatementTimeout, table, droppedIndexesSummary(index, rep.PartitionIndexes))
	return v, nil
}

// droppedIndexesSummary names what a committed blocking drop removed: the
// parent's index and each attached partition index.
func droppedIndexesSummary(index string, partitions []string) string {
	if len(partitions) == 0 {
		return "index " + index + ", which had no partition indexes"
	}
	return fmt.Sprintf("index %s and its %d partition indexes (%s)", index, len(partitions), strings.Join(partitions, ", "))
}

// plannedBlockingIndexDrop returns the index the plan statement drops and
// the definition the plan carried for it, after checking the statement is
// the shape this path runs: a refused, plain, single-index DROP INDEX in the
// plan's own schema that carries the blocking-index-drop execution with
// itself as its one step. Anything else breaches the planner's contract,
// and nothing runs.
func plannedBlockingIndexDrop(report plan.Report, ps plan.Statement, st statement.Statement) (index, definition string, err error) {
	rel := st.IndexRelation()
	if st.Kind() != statement.KindDropIndex || st.Concurrent() || rel.Kind != statement.IndexRelationIndex ||
		rel.Name == "" || rel.Schema != report.Schema || st.IndexTarget() != statement.IndexTargetSingleRelation {
		return "", "", fmt.Errorf("%w: planned blocking drop %q is not a plain DROP INDEX of one index in schema %s",
			executor.ErrInvariantViolation, ps.SQL, report.Schema)
	}
	if ps.Drops == nil || ps.Drops.Kind != schemadiff.DroppedIndex || ps.Drops.Name != rel.Name || ps.Drops.Definition == "" {
		return "", "", fmt.Errorf("%w: planned blocking drop %q carries no definition of index %s",
			executor.ErrInvariantViolation, ps.SQL, rel.Name)
	}
	if ps.Execution != planner.ExecutionBlockingIndexDrop || len(ps.ExecSQL) != 1 || ps.ExecSQL[0] != ps.SQL {
		return "", "", fmt.Errorf("%w: planned blocking drop %q does not run as itself with the blocking-index-drop execution",
			executor.ErrInvariantViolation, ps.SQL)
	}
	return rel.Name, ps.Drops.Definition, nil
}

// blockingIndexDropFailureDetail says what a failed blocking drop left
// behind. Every failure before the commit rolled back, so the index stands;
// a commit the client could not confirm may have landed.
func blockingIndexDropFailureDetail(err error, table, index string) string {
	switch {
	case errors.Is(err, executor.ErrDroppedIndexDrift):
		return fmt.Sprintf("the index changed since the plan was derived, so nothing was dropped on %s; re-plan and review the new plan", table)
	case errors.As(err, new(*executor.BlockingOutcomeUnknownError)):
		return fmt.Sprintf("outcome unknown: the connection or the caller's context ended after DROP INDEX %s was submitted, "+
			"so whether it committed cannot be established from the client — inspect the indexes of %s before retrying", index, table)
	default:
		return fmt.Sprintf("the drop of index %s did not commit and was rolled back, so it still stands on %s and every partition; Code names the outcome",
			index, table)
	}
}

// ranWithoutOnlineSafety reports whether any statement of the run committed
// through the accepted blocking path.
func ranWithoutOnlineSafety(verdicts []verdict.Verdict) bool {
	return slices.ContainsFunc(verdicts, func(v verdict.Verdict) bool {
		return v.Outcome == verdict.OutcomeExecutedWithoutOnlineSafety
	})
}
