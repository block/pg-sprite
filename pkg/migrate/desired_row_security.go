package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// RowSecurityRequest is a desired file that declares row security, for
// RunDesiredWithRowSecurity.
type RowSecurityRequest struct {
	// Schema is the target schema the desired table lives in.
	Schema string
	// Desired is the parsed desired file, table definition and row security.
	Desired statement.DesiredWithRowSecurity
	// ExpectedFingerprint optionally pins the plan, as
	// DesiredRequest.ExpectedFingerprint does. The plan is the table
	// definition's plan, so it is the fingerprint
	// diffplan.PlanWithRowSecurity reported.
	ExpectedFingerprint string
}

// RunDesiredWithRowSecurity converges an existing table onto a desired file
// that declares row security when the live row security already matches the
// file and only the table definition differs. It runs the table definition's
// plan through RunDesired, so every statement keeps RunDesired's admission,
// per-statement gates, and committed-prefix semantics, and it never changes
// the table's row security.
//
// The row security is checked before and after. Before: the plan is derived
// with diffplan.PlanWithRowSecurity, and a file whose row security differs
// from the live table is refused before anything runs. A file that changes
// only the row security is converged by executor.ExecuteDesiredRowSecurity
// instead, which replaces it in one transaction; a file that changes both is
// refused on either path. A table that does not exist is refused too: the
// executor creates it with its row security in one transaction. After: the
// run is executed only when the same comparison, derived again after the
// last commit, is empty. Another session can change the policies while the
// table plan runs; the run then reports that the row security no longer
// matches rather than claiming convergence, and every table statement stays
// committed. The comparison is made again even when the delegated run found
// nothing to do, since the table can change between the two plans. The
// delegated run only converges a table that exists: if the table is dropped
// before it plans, it is refused, so it is never created without its row
// security.
//
// The result-and-error contract is RunDesired's.
func RunDesiredWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, req RowSecurityRequest, opts Options) (DesiredResult, error) {
	// INV: RF-12 — desired-state execution takes no acknowledgement.
	if err := rejectDesiredAcks(opts); err != nil {
		return DesiredResult{}, err
	}
	if err := opts.validate(); err != nil {
		return DesiredResult{}, err
	}
	report, err := diffplan.PlanWithRowSecurity(ctx, pool, req.Schema, req.Desired)
	if refused, ok := rowSecurityPlanRefusal(err); ok {
		return refused, nil
	}
	if err != nil {
		return DesiredResult{}, err
	}
	if len(report.Statements) == 0 {
		return DesiredResult{
			Plan:    report,
			Outcome: verdict.OutcomeExecuted,
			Detail:  "already converged: the live table and its row security match the desired file; nothing to run",
		}, nil
	}
	result, err := runDesired(ctx, pool, DesiredRequest{
		Schema: req.Schema, Desired: req.Desired.TableDefinition(), ExpectedFingerprint: req.ExpectedFingerprint,
	}, opts, refuseDroppedTable)
	if err != nil || result.Outcome != verdict.OutcomeExecuted {
		return result, err
	}
	// INV: ST-12 — executed only when the comparison the next run would
	// make, row security included, is empty after the last commit.
	convergence, err := verifyRowSecurityConverged(ctx, pool, req)
	if len(result.Plan.Statements) == 0 {
		// The table matched its definition when the delegated run planned,
		// so nothing ran. Another session changed the table since this run
		// planned, and may have changed the row security too.
		if err != nil {
			result.Outcome = verdict.OutcomeFailed
			result.Convergence = &convergence
			result.Detail = fmt.Sprintf("nothing was executed: the table already matches its definition, but the file as a whole does not match: %v", err)
			return result, err
		}
		result.Detail = "already converged: the live table and its row security match the desired file; nothing to run"
		return result, nil
	}
	result.Convergence = &convergence
	if err != nil {
		return result.unproven(len(result.Plan.Statements), err)
	}
	result.Detail = fmt.Sprintf("converged: all %d planned statements committed, a fresh plan is empty, and the row security still matches the file", len(result.Plan.Statements))
	return result, nil
}

// refuseDroppedTable refuses a delegated run whose plan finds the table
// missing: the table definition on its own would create the table without
// its row security, so the table is left for the executor that creates it
// with its row security in one transaction.
func refuseDroppedTable(report plan.Report) DesiredResult {
	return DesiredResult{}.refused(plan.RowSecurityMissingTableRefusal(),
		fmt.Sprintf("%s.%s was dropped after its row security was compared; create it with its row security through executor.ExecuteDesiredRowSecurity; nothing was executed", report.Schema, report.Table))
}

// rowSecurityPlanRefusal classifies a planning error that refuses the run
// before anything executes. A row security delta is refused because this
// path never changes row security; a missing table because the executor
// creates it together with its row security.
func rowSecurityPlanRefusal(err error) (DesiredResult, bool) {
	var review *diffplan.RowSecurityReviewRequired
	switch {
	case err == nil:
		return DesiredResult{}, false
	case errors.As(err, &review):
		return DesiredResult{}.refused(plan.RowSecurityReviewRefusal(),
			fmt.Sprintf("%v; the row security differs from the file, and this path changes only the table: converge the row security first with executor.ExecuteDesiredRowSecurity; nothing was executed", err)), true
	case errors.Is(err, schemadiff.ErrTableNotFound):
		return DesiredResult{}.refused(plan.RowSecurityMissingTableRefusal(),
			fmt.Sprintf("%v; create the table with its row security through executor.ExecuteDesiredRowSecurity; nothing was executed", err)), true
	}
	if ref, ok := desiredShapeRefusal(err); ok {
		return DesiredResult{}.refused(ref, fmt.Sprintf("%v; nothing was planned or executed", err)), true
	}
	return DesiredResult{}, false
}

// verifyRowSecurityConverged derives the row-security plan again and returns
// a nil error only when it is empty: the table matches its definition and
// the live row security still matches the file.
func verifyRowSecurityConverged(ctx context.Context, pool *pgxpool.Pool, req RowSecurityRequest) (Convergence, error) {
	again, err := diffplan.PlanWithRowSecurity(ctx, pool, req.Schema, req.Desired)
	if _, ok := errors.AsType[*diffplan.RowSecurityReviewRequired](err); ok {
		return Convergence{Status: ConvergenceUnverifiable},
			fmt.Errorf("%w: the row security on %s.%s no longer matches the file: %w", ErrNotConverged, req.Schema, req.Desired.Table(), err)
	}
	if err != nil {
		return Convergence{Status: ConvergenceUnverifiable}, fmt.Errorf("derive the plan again to verify convergence: %w", err)
	}
	if len(again.Statements) == 0 {
		return Convergence{Status: ConvergenceVerified}, nil
	}
	remaining := make([]string, len(again.Statements))
	for i, ps := range again.Statements {
		remaining[i] = ps.SQL
	}
	return Convergence{Status: ConvergenceRemaining, Remaining: &again},
		fmt.Errorf("%w; a rerun would plan: %s", ErrNotConverged, strings.Join(remaining, "; "))
}
