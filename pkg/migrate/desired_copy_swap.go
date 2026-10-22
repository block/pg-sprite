package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// admitCopySwap is the second half of desired-state admission, for a plan
// the router sends in part through the copy-and-swap backend. The router
// decides the route from the statement alone; whether the route can run is
// a fact about the table and the server — a primary key, no foreign keys or
// triggers or dependent views, logical decoding or a quiesced declaration,
// disk for the shadow — that only the route's preflight reads. Read it
// here, before any statement runs, so a plan the preflight would refuse is
// refused whole and the native statements planned ahead of the retype do
// not commit first and leave the table half-converged.
//
// A plan with no copy-and-swap statement is admitted without a read. The
// preflight is run once: every statement in a plan is on the one table the
// plan converges. The route runs the same preflight again at execution, in
// the session that executes, and that reading is the one that decides; this
// one keeps admission all-or-nothing.
//
// Two shapes, as RunDesired's: a refusal is the refused result with a nil
// error; a stop — the route's connection not wired, which is
// ErrCopySwapConnectionRequired, wired to another database than the pool,
// which is a CopySwapConnectionMismatchError, or a preflight that could
// not finish — is a failed result carrying the plan and no verdict, with
// its error. Nothing has executed in either case and the detail says so.
func admitCopySwap(ctx context.Context, pool *pgxpool.Pool, report plan.Report, opts Options) (DesiredResult, bool, error) {
	first, routed := firstCopySwapStatement(report)
	if !routed {
		return DesiredResult{}, true, nil
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
		var mismatch *CopySwapConnectionMismatchError
		if errors.As(err, &mismatch) {
			return stopped("the route's connection reaches another database than the pool", err)
		}
		return stopped("the route's connection could not be checked against the pool", err)
	}
	_, err := preflight.CheckCopySwap(ctx, pool, report.Schema, report.Table, opts.CopySwap.environment())
	if err == nil {
		return DesiredResult{}, true, nil
	}
	r, detail, err := classifyCopySwapPreflight(err)
	if err != nil {
		return stopped("the route's preflight could not finish", err)
	}
	return DesiredResult{Plan: report}.refused(r, fmt.Sprintf(
		"planned statement %d (%s) routes to copy-and-swap, whose preflight refused the table: %s; nothing was executed",
		first+1, report.Statements[first].SQL, detail)), false, nil
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
