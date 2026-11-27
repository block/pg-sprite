package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/verdict"
)

// AdmitPlanPartitionedForeignKeys is the part of desired-state admission for
// a plan that adds a foreign key to a partitioned table as the partitioned
// foreign key build. The plan routes the statement to the build from its
// text alone; whether the build takes it is a fact about the catalog — the
// referenced table and the partition tree — that the build reads under its
// table lock when the run reaches the statement, after the statements
// planned ahead of it have committed. Read it here, through the build's own
// checks (executor.AdmitPartitionedForeignKey), before any statement runs,
// so a foreign key the build would refuse stops the plan whole and the
// table is not left half-converged. The build makes the same checks again
// when it runs; that reading is the one that decides.
//
// A plan with no partitioned foreign key build is admitted without a read.
// [RunDesired] calls it, and so does a desired-state dry run, to report the
// result the run gives. The shapes are [RunDesired]'s: a refused result with
// a nil error is the build's refusal — a referenced table the build does not
// take, or a sub-partitioned partition — classified as the build's refusal
// verdict is; a failed result carrying the plan and no verdict, with its
// error, is a tree the build fails on (a foreign table partition, a
// partition whose detach has not finished) or a catalog read that could not
// finish. Nothing has executed in either case and the detail says so. The
// boolean is false when the plan is not admitted.
func AdmitPlanPartitionedForeignKeys(ctx context.Context, pool *pgxpool.Pool, report plan.Report) (DesiredResult, bool, error) {
	for i, ps := range report.Statements {
		if ps.Execution != planner.ExecutionPartitionedForeignKeyBuild {
			continue
		}
		err := executor.AdmitPartitionedForeignKey(ctx, pool, ps.SQL)
		if err == nil {
			continue
		}
		if r, ok := partitionedForeignKeyCatalogRefusal(err); ok {
			return DesiredResult{Plan: report}.refused(r, fmt.Sprintf(
				"planned statement %d (%s) adds a foreign key to a partitioned table, which the engine cannot run safely: %v; nothing was executed",
				i+1, ps.SQL, err)), false, nil
		}
		return DesiredResult{
			Plan:    report,
			Outcome: verdict.OutcomeFailed,
			Detail: fmt.Sprintf("planned statement %d of %d adds a foreign key to a partitioned table, and the check of its referenced table and partition tree stopped: %v; nothing was executed",
				i+1, len(report.Statements), err),
		}, false, fmt.Errorf("planned statement %d adds a foreign key to a partitioned table: %w", i+1, err)
	}
	return DesiredResult{}, true, nil
}
