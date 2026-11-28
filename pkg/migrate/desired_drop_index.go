package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// dropPlannedIndex runs one index drop of an admitted desired-state plan,
// statement ps at the given step. The imperative front door has no online
// path for it: its only way through is the blocking DROP INDEX, which holds
// the table's ACCESS EXCLUSIVE lock for the whole drop. A plan's index drop
// runs as the DROP INDEX CONCURRENTLY the plan names in ps.ExecSQL, through
// the executor's re-check of the index immediately before the drop.
//
// The caller has admitted the plan against the acknowledgement and the
// pinned fingerprint, so this audits the acknowledgement and reports it on
// the verdict, as [Run] does for an acknowledged ALTER TABLE drop. A
// copy-and-swap run stopped on the table refuses the drop before it is
// audited, as Run refuses any statement but the stopped one. A replica
// identity index ends in the refusal verdict a native drop gets at its
// re-check; every other failure is a failed verdict and an error, and the
// verdict says whether the index may still stand.
func dropPlannedIndex(ctx context.Context, pool *pgxpool.Pool, report plan.Report, ps plan.Statement,
	st statement.Statement, opts Options, step int) (verdict.Verdict, error) {
	index, definition, err := plannedIndexDrop(report, ps, st)
	if err != nil {
		return verdict.Verdict{}, err
	}
	table := desiredTable(report)
	heldVerdict, held, err := refuseOverStoppedRun(ctx, pool, st, report.Schema, report.Table)
	if err != nil {
		return verdict.Verdict{}, err
	}
	if held {
		return heldVerdict, nil
	}
	auditAllowDestructive(opts.audit(), st, table)

	opts.startStep(step, ps.SQL)
	var rep executor.DroppedIndex
	if ps.Replacement != nil {
		rep, err = executor.ExecuteReplacedIndexDropConcurrently(ctx, pool, report.Schema, report.Table, index, definition, *ps.Replacement, opts.Budget.Concurrent)
	} else {
		rep, err = executor.ExecuteDropIndexConcurrently(ctx, pool, report.Schema, report.Table, index, definition, opts.Budget.Concurrent)
	}
	logger := opts.logger()
	switch {
	case err == nil:
		logger.Debug("planned index dropped concurrently",
			"table", table, "index", index, "elapsed", rep.Duration)
		return verdict.Verdict{
			Outcome:            verdict.OutcomeExecuted,
			Statement:          ps.SQL,
			Table:              table,
			AllowedDestructive: true,
			ExecutedSQL:        slices.Clone(ps.ExecSQL),
			Detail: fmt.Sprintf("dropped with DROP INDEX CONCURRENTLY in %s: %s was not locked against reads or writes for the drop",
				rep.Duration.Round(time.Millisecond), table),
		}, nil
	case errors.Is(err, executor.ErrReplicaIdentityDrop):
		v := admissionRefusalVerdict(st, err, verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), false)
		v.Table = table
		v.AllowedDestructive = true
		return v, nil
	}
	v := failureVerdict(st, err, executor.SequenceReport{}, false)
	v.Table = table
	v.AllowedDestructive = true
	v.Detail = droppedIndexFailureDetail(err, table, index)
	v = WithServerError(v, err)
	logger.Debug("planned index drop failed", "table", table, "index", index, "code", v.Code)
	return v, fmt.Errorf("drop index %s on %s: %w", index, table, err)
}

// plannedIndexDrop returns the name of the index the plan statement drops
// and the definition the plan carried for it, after checking the statement
// is the shape this path runs: a single-index DROP INDEX in the plan's own
// schema whose execution form is exactly one DROP INDEX CONCURRENTLY of the
// same index. Anything else breaches the planner's contract, and nothing
// runs.
func plannedIndexDrop(report plan.Report, ps plan.Statement, st statement.Statement) (index, definition string, err error) {
	rel := st.IndexRelation()
	if st.Kind() != statement.KindDropIndex || rel.Kind != statement.IndexRelationIndex || rel.Name == "" || rel.Schema != report.Schema {
		return "", "", fmt.Errorf("%w: planned statement %q is not a DROP INDEX of one index in schema %s",
			executor.ErrInvariantViolation, ps.SQL, report.Schema)
	}
	if ps.Drops == nil || ps.Drops.Kind != schemadiff.DroppedIndex || ps.Drops.Name != rel.Name || ps.Drops.Definition == "" {
		return "", "", fmt.Errorf("%w: planned index drop %q carries no definition of index %s",
			executor.ErrInvariantViolation, ps.SQL, rel.Name)
	}
	if err := checkPlannedReplacement(ps); err != nil {
		return "", "", err
	}
	if len(ps.ExecSQL) != 1 {
		return "", "", fmt.Errorf("%w: planned index drop %q carries %d execution statements, not the one DROP INDEX CONCURRENTLY",
			executor.ErrInvariantViolation, ps.SQL, len(ps.ExecSQL))
	}
	exec, err := statement.ParseOne(ps.ExecSQL[0])
	if err != nil {
		return "", "", fmt.Errorf("%w: planned index drop %q has an execution statement its own parser rejects: %w",
			executor.ErrInvariantViolation, ps.SQL, err)
	}
	execRel := exec.IndexRelation()
	if exec.Kind() != statement.KindDropIndex || !exec.Concurrent() || execRel != rel {
		return "", "", fmt.Errorf("%w: planned index drop %q executes as %q, not as DROP INDEX CONCURRENTLY of the same index",
			executor.ErrInvariantViolation, ps.SQL, ps.ExecSQL[0])
	}
	return rel.Name, ps.Drops.Definition, nil
}

// checkPlannedReplacement refuses a drop whose replacement is named without
// being carried, or carried under another name. A drop that names its
// replacement runs only through the check that the replacement is still in
// place, so one that reaches here without the replacement it names, such as
// a plan rebuilt from its JSON report, which leaves the replacement out,
// would otherwise drop the old index or constraint unchecked.
func checkPlannedReplacement(ps plan.Statement) error {
	switch {
	case ps.ReplacedBy == "" && ps.Replacement == nil:
		return nil
	case ps.Replacement == nil:
		return fmt.Errorf("%w: planned drop %q is replaced by %s but carries no replacement to check before the drop",
			executor.ErrInvariantViolation, ps.SQL, ps.ReplacedBy)
	case ps.Replacement.Index != ps.ReplacedBy:
		return fmt.Errorf("%w: planned drop %q is replaced by %q but carries the replacement %q",
			executor.ErrInvariantViolation, ps.SQL, ps.ReplacedBy, ps.Replacement.Index)
	}
	return nil
}

// droppedWithReplacement returns what a destructive statement other than an index
// drop removes, with the replacement the plan carries for it attached: a
// unique constraint the plan replaces with a standalone index is dropped
// only while that index is still in place. A replacement on any other drop
// breaches the planner's contract, and nothing runs.
func droppedWithReplacement(ps plan.Statement) (*schemadiff.DroppedObject, error) {
	if err := checkPlannedReplacement(ps); err != nil {
		return nil, err
	}
	if ps.Replacement == nil {
		return ps.Drops, nil
	}
	if ps.Kind != schemadiff.ChangeDropConstraint || ps.Drops == nil || ps.Drops.Kind != schemadiff.DroppedConstraint {
		return nil, fmt.Errorf("%w: planned statement %q is replaced by %s but drops no unique constraint",
			executor.ErrInvariantViolation, ps.SQL, ps.Replacement.Index)
	}
	dropped := *ps.Drops
	dropped.Replacement = ps.Replacement
	return &dropped, nil
}

// droppedIndexFailureDetail says what a failed concurrent index drop left
// behind: nothing when the read before it refused, a different index than
// the one reviewed when the executor found that after the fact, and an
// index that may still stand or be invalid when the budget or the caller
// cancelled it.
func droppedIndexFailureDetail(err error, table, index string) string {
	_, overBudget := errors.AsType[*executor.BudgetError](err)
	switch {
	case errors.Is(err, executor.ErrDroppedIndexDrift):
		return fmt.Sprintf("the index changed since the plan was derived, so nothing was dropped on %s; re-plan and review the new plan", table)
	case errors.Is(err, executor.ErrReplacementIndexDrift):
		return fmt.Sprintf("the index built to replace %s changed since the plan was derived, so nothing was dropped on %s; "+
			"inspect %s_new: when it is gone, re-plan, which builds it again; when it is invalid or has another definition and no build of it is running, "+
			"drop it with DROP INDEX CONCURRENTLY, then re-plan", index, table, index)
	case errors.Is(err, executor.ErrDroppedIndexReplaced):
		return fmt.Sprintf("an index the plan did not review was dropped: another session gave the name %s to a different index between the check and the drop; "+
			"inspect the indexes of %s and recreate the dropped one if it should stay", index, table)
	case errors.Is(err, executor.ErrCancelledByCaller), errors.Is(err, executor.ErrCancelledExternally), overBudget:
		return fmt.Sprintf("the drop did not finish: index %s on %s may still stand, or the cancelled drop may have left it invalid; "+
			"inspect the table's indexes, and an invalid leftover shows in the next plan as an unfinished index build", index, table)
	default:
		return fmt.Sprintf("execution failed before the drop's outcome was confirmed; inspect the indexes of %s for %s before retrying", table, index)
	}
}

// checkDropRoute refuses a statement that carries a reviewed drop but is
// routed anywhere other than the native route. A reviewed drop runs only on
// a route that reads the object again right before it drops it.
func checkDropRoute(rs router.Statement, dropped *schemadiff.DroppedObject) error {
	if dropped == nil || rs.Disposition != router.DispositionExecute || rs.Backend == router.BackendNative {
		return nil
	}
	return fmt.Errorf("%w: a %s route cannot carry the drop of %s %s",
		executor.ErrInvariantViolation, rs.Backend, dropped.Kind, dropped.Name)
}
