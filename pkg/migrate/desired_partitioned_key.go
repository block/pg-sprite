package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
)

// AdmitPlanPartitionedKeys is the catalog half of the partitioned key
// build's admission ([AdmitPartitionedKeyCatalog]) for a desired-state plan,
// read from the live catalog on pool for every statement the plan runs as
// the partitioned key build. A key that does not include every partition key
// column of every partitioned table under the target, and a tree with a
// sub-partitioned partition, are refused here before any statement runs, so
// the statements planned ahead of the build do not commit first and leave
// the table without the key it was promised. The build's checks of the
// parent, which it reads under its table lock, are not part of it.
//
// A plan with no partitioned key build is admitted without a read. The
// result, the boolean, and the error mean what they mean for
// [AdmitPlanCopySwapPartitioning]: the boolean is true when the plan is
// admitted, and a refusal is a refused result carrying the plan, naming the
// planned statement the build refused. A host that plans through the library
// calls it with a pool from pkg/dbconn.
func AdmitPlanPartitionedKeys(ctx context.Context, pool *pgxpool.Pool, report plan.Report) (DesiredResult, bool, error) {
	for i, ps := range report.Statements {
		if ps.Execution != planner.ExecutionPartitionedKeyBuild {
			continue
		}
		if len(ps.ExecSQL) != 1 {
			return DesiredResult{}, false, fmt.Errorf("planned statement %d: %w: the partitioned key build runs one statement, got %d",
				i+1, executor.ErrInvariantViolation, len(ps.ExecSQL))
		}
		err := executor.AdmitPartitionedKey(ctx, pool, ps.ExecSQL[0])
		if err == nil {
			continue
		}
		r, ok := partitionedKeyCatalogRefusal(err)
		if !ok {
			return DesiredResult{}, false, fmt.Errorf("planned statement %d adds a key constraint to a partitioned table: %w", i+1, err)
		}
		return DesiredResult{Plan: report}.refused(r, fmt.Sprintf(
			"planned statement %d (%s) adds a key constraint to a partitioned table, which the build refuses: %v; nothing was executed",
			i+1, ps.SQL, err)), false, nil
	}
	return DesiredResult{}, true, nil
}
