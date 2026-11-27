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

// partitionedForeignKeyBuild runs an ADD FOREIGN KEY on a partitioned table,
// on a server before PostgreSQL 18, as the partitioned foreign key build and
// ends in one verdict. The statement is the request itself, not the
// planner's steps: the substitute the planner made for a plain table, ADD
// ... NOT VALID then VALIDATE, cannot run on a partitioned parent before
// version 18. The partitions are listed when the build runs. A failure
// leaves the partition foreign keys finished so far; running the same
// statement again resumes from them, and the failed verdict says so.
//
// The build's static admission refusals are refusals with nothing executed.
// Its catalog refusals of the parent, the referenced table, and the tree
// (partitionedForeignKeySentinels) are refusals only when the build gave no
// partition the foreign key before it stopped; after that they are failed
// verdicts, because something committed.
func partitionedForeignKeyBuild(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable,
	st statement.Statement, opts Options, step int, logger *slog.Logger) (verdict.Verdict, error) {
	if err := checkPartitionedIndexConnection(ctx, pool, opts); err != nil {
		return verdict.Verdict{}, fmt.Errorf("migrate: %w", err)
	}
	logger.Debug("routing an ADD FOREIGN KEY on a partitioned table to the partitioned foreign key build",
		"table", qualified(st))
	start := time.Now()
	rep, err := executor.AddPartitionedForeignKeyAsStep(ctx, opts.Connection, pool, pt, st.SQL(),
		opts.Budget.Validate, opts.runStep(step))
	elapsed := time.Since(start)
	if err != nil {
		if v, refused := partitionedForeignKeyRefusalVerdict(st, err, rep); refused {
			logger.Debug("partitioned foreign key build refused",
				"reason", string(v.Reason), "class", string(v.Class), "elapsed", elapsed)
			return v, nil
		}
		v := partitionedForeignKeyFailureVerdict(st, err)
		logger.Debug("partitioned foreign key build failed", "code", v.Code, "elapsed", elapsed)
		return v, fmt.Errorf("run schema change on %s: %w", qualified(st), err)
	}
	logger.Debug("partitioned foreign key build committed",
		"table", qualified(st), "constraint", rep.Constraint, "partitions", rep.Partitions,
		"leaves", len(rep.Leaves), "parent_added", rep.ParentAdded, "elapsed", elapsed)
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    partitionedForeignKeyDetail(rep),
	}, nil
}

// partitionedForeignKeyRefusalVerdict maps a stopped partitioned foreign key
// build to its refusal verdict when the stop belongs to the refusal
// contract: nothing was executed. ok is false for every other stop, which is
// a failed verdict.
func partitionedForeignKeyRefusalVerdict(st statement.Statement, err error, rep executor.PartitionedForeignKeyReport) (verdict.Verdict, bool) {
	if r, ok := admissionRefusal(err); ok {
		return admissionRefusalVerdict(st, err, r, false), true
	}
	r, ok := partitionedForeignKeyRefusal(err)
	if !ok {
		return verdict.Verdict{}, false
	}
	// A leaf joins the report only once its foreign key exists, and the
	// build reads these refusals before it touches a leaf of a pass, so an
	// empty list means nothing was added.
	if len(rep.Leaves) > 0 {
		return verdict.Verdict{}, false
	}
	return admissionRefusalVerdict(st, err, r, false), true
}

// partitionedForeignKeyDetail says what a finished build did, in one
// sentence.
func partitionedForeignKeyDetail(rep executor.PartitionedForeignKeyReport) string {
	if !rep.ParentAdded {
		return fmt.Sprintf("the table is partitioned and already had foreign key %s, adopted on all %d partitions; nothing was changed",
			rep.Constraint, rep.Partitions)
	}
	counts := map[executor.PartitionForeignKeyAction]int{}
	for _, l := range rep.Leaves {
		counts[l.Action]++
	}
	return fmt.Sprintf("the table is partitioned, so pg-sprite gave each partition the foreign key online (NOT VALID, then VALIDATE while writes continued) "+
		"and the parent's ADD CONSTRAINT %s adopted them in one brief transaction that also held ACCESS EXCLUSIVE on the referenced table: "+
		"%d partitions, %d found already validated, %d validated, %d added",
		rep.Constraint, rep.Partitions,
		counts[executor.PartitionForeignKeyFound], counts[executor.PartitionForeignKeyValidated], counts[executor.PartitionForeignKeyAdded])
}

// partitionedForeignKeyFailureVerdict is the failed verdict for a
// partitioned foreign key build that stopped. The build never rolls back
// what it finished, and the error alone does not say how much that was, so
// the detail claims neither that nothing changed nor what remains; it says
// earlier steps may have committed and how to find out, and that running the
// same statement again resumes from whatever the build had finished.
func partitionedForeignKeyFailureVerdict(st statement.Statement, err error) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.OutcomeCode(err)),
		Statement: st.SQL(),
		Table:     qualified(st),
	}
	if _, commitUnknown := errors.AsType[*executor.CommitOutcomeUnknownError](err); commitUnknown {
		v.Detail = fmt.Sprintf("outcome unknown: a COMMIT did not return success, so whether the last step of the partitioned foreign key build committed "+
			"cannot be established from the client; inspect the foreign keys of %s and its partitions, then run the same statement again to resume", v.Table)
		return v
	}
	if executor.Code(v.Code) == executor.CodePartitionedForeignKeyNotValidSupported {
		// Permanent for the build, but not for this front door: Run routes a
		// rerun on PostgreSQL 18 to the ordinary NOT VALID then VALIDATE route.
		v.Detail = fmt.Sprintf("the server became PostgreSQL 18 or later after the statement was routed to the partitioned foreign key build, "+
			"which that version does not need; run the same statement again, and it takes the ordinary NOT VALID then VALIDATE route on %s", v.Table)
		return v
	}
	if executor.Code(v.Code).Permanent() {
		// A permanent cause fails a rerun the same way until it is fixed.
		v.Detail = fmt.Sprintf("the partitioned foreign key build stopped on a cause that a rerun does not clear (%s); steps before the stop may have committed, "+
			"so inspect the foreign keys of %s and its partitions, fix the cause the error names, then run the same statement again to resume from whatever it had finished", v.Code, v.Table)
		return v
	}
	v.Detail = fmt.Sprintf("the partitioned foreign key build stopped before it finished; steps before the stop may have committed, "+
		"so inspect the foreign keys of %s and its partitions, then run the same statement again to resume from whatever it had finished", v.Table)
	return v
}
