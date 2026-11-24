package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// partitionedKeyBuild runs an ADD PRIMARY KEY or ADD UNIQUE on a partitioned
// table as the partitioned key build and ends in one verdict. The statement is
// the request itself, not the planner's steps: the substitute the planner made
// for a plain table, a concurrent unique build and ADD CONSTRAINT ... USING
// INDEX, cannot run on a partitioned parent. The partitions are listed when
// the build runs. A failure leaves the partition constraints finished so far;
// running the same statement again resumes from them, and the failed verdict
// says so.
//
// The build's static admission refusals, and the partition key check it makes
// before it touches anything, are refusals with nothing executed. Its other
// catalog refusals (the parent stands in the way, a primary key column allows
// NULL, a sub-partitioned partition) are refusals only when the build gave no
// partition the key before it stopped; after that they are failed verdicts,
// because something committed.
func partitionedKeyBuild(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable,
	st statement.Statement, opts Options, step int, logger *slog.Logger) (verdict.Verdict, error) {
	if err := checkPartitionedIndexConnection(ctx, pool, opts); err != nil {
		return verdict.Verdict{}, fmt.Errorf("migrate: %w", err)
	}
	logger.Debug("routing an ADD PRIMARY KEY or ADD UNIQUE on a partitioned table to the partitioned key build",
		"table", qualified(st))
	start := time.Now()
	rep, err := executor.AddPartitionedKeyAsStep(ctx, opts.Connection, pool, pt, st.SQL(),
		opts.Budget.Concurrent, opts.runStep(step))
	elapsed := time.Since(start)
	if err != nil {
		if v, refused := partitionedKeyRefusalVerdict(st, err, rep); refused {
			logger.Debug("partitioned key build refused",
				"reason", string(v.Reason), "class", string(v.Class), "elapsed", elapsed)
			return v, nil
		}
		v := partitionedKeyFailureVerdict(st, err)
		logger.Debug("partitioned key build failed", "code", v.Code, "elapsed", elapsed)
		return v, fmt.Errorf("run schema change on %s: %w", qualified(st), err)
	}
	logger.Debug("partitioned key build committed",
		"table", qualified(st), "constraint", rep.Constraint, "partitions", rep.Partitions,
		"leaves", len(rep.Leaves), "parent_added", rep.ParentAdded, "elapsed", elapsed)
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    partitionedKeyDetail(rep),
	}, nil
}

// partitionedKeyRefusalVerdict maps a stopped partitioned key build to its
// refusal verdict when the stop belongs to the refusal contract: nothing was
// executed. ok is false for every other stop, which is a failed verdict.
func partitionedKeyRefusalVerdict(st statement.Statement, err error, rep executor.PartitionedKeyReport) (verdict.Verdict, bool) {
	if r, ok := admissionRefusal(err); ok {
		return admissionRefusalVerdict(st, err, r, false), true
	}
	r, ok := partitionedKeyRefusal(err)
	if !ok {
		return verdict.Verdict{}, false
	}
	// A leaf joins the report only once its key constraint exists, so an
	// empty list means the build stopped before it gave any partition the
	// key, at the checks it makes first.
	if len(rep.Leaves) > 0 {
		return verdict.Verdict{}, false
	}
	return admissionRefusalVerdict(st, err, r, false), true
}

// partitionedKeyDetail says what a finished build did, in one sentence.
func partitionedKeyDetail(rep executor.PartitionedKeyReport) string {
	if !rep.ParentAdded {
		return fmt.Sprintf("the table is partitioned and already had constraint %s, adopted on all %d partitions; nothing was changed",
			rep.Constraint, rep.Partitions)
	}
	counts := map[executor.PartitionKeyAction]int{}
	for _, l := range rep.Leaves {
		counts[l.Action]++
	}
	return fmt.Sprintf("the table is partitioned, so pg-sprite gave each partition the key constraint online and the parent's ADD CONSTRAINT %s adopted them: "+
		"%d partitions, %d built, %d constrained from an existing index, %d found already constrained",
		rep.Constraint, rep.Partitions,
		counts[executor.PartitionKeyBuilt], counts[executor.PartitionKeyConstrained], counts[executor.PartitionKeyFound])
}

// partitionedKeyFailureVerdict is the failed verdict for a partitioned key
// build that stopped. The build never rolls back what it finished, and the
// error alone does not say how much that was, so the detail claims neither
// that nothing changed nor what remains; it says earlier steps may have
// committed and how to find out, and that running the same statement again
// resumes from whatever the build had finished.
func partitionedKeyFailureVerdict(st statement.Statement, err error) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.OutcomeCode(err)),
		Statement: st.SQL(),
		Table:     qualified(st),
	}
	if _, commitUnknown := errors.AsType[*executor.CommitOutcomeUnknownError](err); commitUnknown {
		v.Detail = fmt.Sprintf("outcome unknown: a COMMIT did not return success, so whether the last step of the partitioned key build committed "+
			"cannot be established from the client; inspect the constraints and indexes of %s and its partitions, then run the same statement again to resume", v.Table)
		return v
	}
	v.Detail = fmt.Sprintf("the partitioned key build stopped before it finished; steps before the stop may have committed, "+
		"so inspect the constraints and indexes of %s and its partitions, then run the same statement again to resume from whatever it had finished", v.Table)
	return v
}
