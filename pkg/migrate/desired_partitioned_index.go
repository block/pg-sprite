package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/verdict"
)

// admitPartitionedIndexBuild is the part of desired-state admission for a
// plan that builds an index on a partitioned table. The build needs
// Options.Connection, wired to the pool's database, which only a run that
// reaches the statement would otherwise find out. Prove it here, before any
// statement runs, so a missing or misdirected connection stops the plan whole
// and the statements planned ahead of the index do not commit first and
// leave the table half-converged. The build checks the same wiring again when
// it runs.
//
// A plan with no partitioned index build is admitted without a read. A stop
// is a failed result carrying the plan and no verdict, with its error: the
// connection not wired is [ErrConnectionRequired], wired to another database
// than the pool is a [PartitionedIndexConnectionMismatchError], and a connection that
// could not be checked is an ordinary error. Nothing has executed.
func admitPartitionedIndexBuild(ctx context.Context, pool *pgxpool.Pool, report plan.Report, opts Options) (DesiredResult, bool, error) {
	first, builds := firstPartitionedIndexBuild(report)
	if !builds {
		return DesiredResult{}, true, nil
	}
	stopped := func(what string, err error) (DesiredResult, bool, error) {
		return DesiredResult{
			Plan:    report,
			Outcome: verdict.OutcomeFailed,
			Detail: fmt.Sprintf("planned statement %d of %d builds an index on a partitioned table and %s; nothing was executed",
				first+1, len(report.Statements), what),
		}, false, fmt.Errorf("planned statement %d builds an index on a partitioned table: %w", first+1, err)
	}
	err := checkPartitionedIndexConnection(ctx, pool, opts)
	switch {
	case err == nil:
		return DesiredResult{}, true, nil
	case errors.Is(err, ErrConnectionRequired):
		return stopped("Options.Connection is not configured", err)
	}
	if _, ok := errors.AsType[*PartitionedIndexConnectionMismatchError](err); ok {
		return stopped("Options.Connection reaches another database than the pool", err)
	}
	return stopped("Options.Connection could not be checked against the pool", err)
}

// firstPartitionedIndexBuild returns the position of the first planned
// statement that runs as the partitioned index build, and whether there is
// one.
func firstPartitionedIndexBuild(report plan.Report) (int, bool) {
	for i, ps := range report.Statements {
		if ps.Execution == planner.ExecutionPartitionedIndexBuild {
			return i, true
		}
	}
	return 0, false
}
