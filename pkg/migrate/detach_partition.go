package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// AdmitDetachPartition is the catalog half of the admission of a DETACH
// PARTITION ... CONCURRENTLY step, for steps [AdmitRoutedSteps] admitted: a
// concurrent detach from a table that has a DEFAULT partition, which
// PostgreSQL rejects, is refused from the live catalog on pool, as [Run]
// refuses it before any step executes. Every other step is admitted. The
// verdict, the boolean, and the error mean what they mean for
// [AdmitRoutedSteps].
func AdmitDetachPartition(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, schema string, steps []string) (verdict.Verdict, bool, error) {
	return admitRouted(st, schema, steps, func(steps []string) error {
		for _, sql := range steps {
			if err := executor.AdmitDetachPartition(ctx, pool, sql); err != nil {
				return err
			}
		}
		return nil
	})
}

// detachDetail is the executed verdict's detail for a run whose steps
// detached a partition concurrently; ok is false when none did.
func detachDetail(rep executor.SequenceReport, b executor.Budget) (string, bool) {
	for _, step := range rep.Steps {
		if step.Kind != executor.StepDetachPartition {
			continue
		}
		if step.FinishedPendingDetach {
			return fmt.Sprintf("the partition was left detach-pending by an earlier run; pg-sprite finished the detach "+
				"with DETACH PARTITION ... FINALIZE within budgets (lock %s, statement %s)",
				b.LockTimeout, b.StatementTimeout), true
		}
		return fmt.Sprintf("detached the partition concurrently within budgets (lock %s, statement %s): reads and "+
			"writes of the table's other partitions kept running, and the partition is now a table of its own",
			b.LockTimeout, b.StatementTimeout), true
	}
	return "", false
}
