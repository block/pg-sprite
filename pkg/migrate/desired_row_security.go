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
// committed.
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
	result, err := RunDesired(ctx, pool, DesiredRequest{
		Schema: req.Schema, Desired: req.Desired.TableDefinition(), ExpectedFingerprint: req.ExpectedFingerprint,
	}, opts)
	if err != nil || result.Outcome != verdict.OutcomeExecuted {
		return result, err
	}
	if result.Convergence == nil {
		// RunDesired found the table already converged and ran nothing,
		// and the row security matched when this run planned.
		return result, nil
	}
	// INV: ST-12 — executed only when the comparison the next run would
	// make, row security included, is empty after the last commit.
	convergence, err := verifyRowSecurityConverged(ctx, pool, req)
	result.Convergence = &convergence
	if err != nil {
		return result.unproven(len(result.Plan.Statements), err)
	}
	result.Detail = fmt.Sprintf("converged: all %d planned statements committed, a fresh plan is empty, and the row security still matches the file", len(result.Plan.Statements))
	return result, nil
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
	var review *diffplan.RowSecurityReviewRequired
	if errors.As(err, &review) {
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
