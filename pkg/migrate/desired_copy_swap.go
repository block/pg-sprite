package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// admitCopySwap is the second half of desired-state admission, for a plan
// the router sends in part through the copy-and-swap backend. The router
// decides the route from the statement alone; whether the route can run is
// a fact about the table and the server — a primary key, no foreign keys or
// triggers or dependent views, logical decoding (desired-state execution
// takes no block-writes acknowledgement, so the route is always live), disk
// for the shadow — that only the route's preflight reads. Read it
// here, before any statement runs, so a plan the preflight would refuse is
// refused whole and the native statements planned ahead of the retype do
// not commit first and leave the table half-converged.
//
// A plan with no copy-and-swap statement is admitted without a read. A
// routed plan whose policy keeps the route closed (CopySwapOptions.Enabled
// unset) is refused whole before the preflight reads anything, with the
// same reason and cause the imperative door gives; planning has already
// read the catalog to produce the plan. The preflight is run
// once: every statement in a plan is on the one table the plan converges.
// The route runs the same preflight again at execution, in the session that
// executes, and that reading is the one that decides; this one keeps
// admission all-or-nothing.
//
// Two shapes, as RunDesired's: a refusal is the refused result with a nil
// error; a stop — the route's connection not wired, which is
// ErrCopySwapConnectionRequired, wired to another database than the pool,
// which is a CopySwapConnectionMismatchError, a pool smaller than
// CopySwapMinConns, which wraps executor.ErrPoolTooSmall, or a preflight
// that could not finish — is a failed result carrying the plan and no verdict, with
// its error. Nothing has executed in either case and the detail says so.
func admitCopySwap(ctx context.Context, pool *pgxpool.Pool, report plan.Report, opts Options) (DesiredResult, bool, error) {
	first, routed := firstCopySwapStatement(report)
	if !routed {
		return DesiredResult{}, true, nil
	}
	if !opts.CopySwap.Enabled {
		return DesiredResult{Plan: report}.refused(copySwapNotEnabledRefusal(), fmt.Sprintf(
			"planned statement %d (%s) routes to copy-and-swap, which is not enabled for this run: %s",
			first+1, report.Statements[first].SQL, copySwapNotEnabledDetail)), false, nil
	}
	stopped := func(what string, err error) (DesiredResult, bool, error) {
		return DesiredResult{
			Plan:    report,
			Outcome: verdict.OutcomeFailed,
			Detail: fmt.Sprintf("planned statement %d of %d routes to copy-and-swap and %s; nothing was executed",
				first+1, len(report.Statements), what),
		}, false, fmt.Errorf("planned statement %d routes to copy-and-swap: %w", first+1, err)
	}
	if opts.CopySwap.Connection.URL == "" {
		return stopped("the route's connection is not configured", ErrCopySwapConnectionRequired)
	}
	if err := checkCopySwapConnection(ctx, pool, opts.CopySwap.Connection); err != nil {
		if _, ok := errors.AsType[*CopySwapConnectionMismatchError](err); ok {
			return stopped("the route's connection reaches another database than the pool", err)
		}
		return stopped("the route's connection could not be checked against the pool", err)
	}
	if err := schemachange.CheckPool(pool, opts.CopySwap.Copier); err != nil {
		return stopped("the pool is too small for the route", err)
	}
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	if err != nil {
		return stopped("the route's checkpoint store could not be opened", err)
	}
	_, err = checkCopySwap(ctx, pool, store, report.Schema, report.Table, opts.CopySwap)
	if err == nil {
		return DesiredResult{}, true, nil
	}
	noLogicalDecoding := preflight.CopySwapRefusalCauseOf(err) == preflight.CopySwapCauseLogicalDecodingUnavailable
	r, detail, err := classifyCopySwapPreflight(err)
	if err != nil {
		return stopped("the route's preflight could not finish", err)
	}
	if noLogicalDecoding {
		// Desired-state execution takes no block-writes acknowledgement,
		// so the offline route is the imperative door, one statement at a
		// time.
		detail += fmt.Sprintf("; to run offline instead, blocking the table's writes until each swap, run each planned statement with migrate --alter --block-writes %s.%s",
			report.Schema, report.Table)
	}
	return refuseCopySwapPlan(report, first, r, detail), false, nil
}

// AdmitPlanCopySwapPartitioning is [AdmitCopySwapPartitioning] for a
// desired-state plan: when a planned statement routes to
// copy-and-swap and the table is a partitioned parent, a partition, or part
// of an inheritance tree, it returns the whole-plan refusal [RunDesired]
// gives when the route's preflight refuses the table at admission — reason
// copy-and-swap-refused, cause copy-and-swap-partitioned (or the unlogged or
// forced row security cause the preflight decides first), the plan
// carried — read from the live catalog on pool. It is how a desired-state dry run
// learns that refusal instead of previewing a plan the run turns down; like
// [AdmitCopySwapPartitioning] it claims nothing else the run's admission
// decides. A plan with no copy-and-swap statement, which a plan that
// creates its table never has, is admitted without a read. The boolean is
// false when it refuses, as for [AdmitPlan] and the opposite sense of
// [AdmitCopySwapPartitioning]; a non-nil error is a catalog read that could not
// finish, and nothing has been decided.
func AdmitPlanCopySwapPartitioning(ctx context.Context, pool *pgxpool.Pool, report plan.Report) (DesiredResult, bool, error) {
	first, routed := firstCopySwapStatement(report)
	if !routed {
		return DesiredResult{}, true, nil
	}
	err := preflight.CheckCopySwapPartitioning(ctx, pool, report.Schema, report.Table)
	if err == nil {
		return DesiredResult{}, true, nil
	}
	r, detail, err := classifyCopySwapPreflight(err)
	if err != nil {
		return DesiredResult{}, false, fmt.Errorf("planned statement %d routes to copy-and-swap: %w", first+1, err)
	}
	return refuseCopySwapPlan(report, first, r, detail), false, nil
}

// refuseCopySwapPlan is the whole-plan refusal for a table the route's
// preflight refused, naming the first planned statement that routes to it.
func refuseCopySwapPlan(report plan.Report, first int, r verdict.Refusal, detail string) DesiredResult {
	return DesiredResult{Plan: report}.refused(r, fmt.Sprintf(
		"planned statement %d (%s) routes to copy-and-swap, whose preflight refused the table: %s; nothing was executed",
		first+1, report.Statements[first].SQL, detail))
}

// firstCopySwapStatement returns the position of the first planned
// statement the router assigned to the copy-and-swap backend, and whether
// there is one.
func firstCopySwapStatement(report plan.Report) (int, bool) {
	for i, ps := range report.Statements {
		if ps.Backend == router.BackendCopyAndSwap {
			return i, true
		}
	}
	return 0, false
}
