package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// acquireTableLock takes the table's change lock for this process. A lock
// another instance holds is a refusal, not an error: nothing was executed
// and the caller may retry once the other instance finishes. Every other
// failure — including a connection that cannot keep one server session, so
// the lock would exclude nobody — stops the pipeline before it reaches a
// verdict.
//
// INV: LK-1 — at most one schema change runs per table.
func acquireTableLock(ctx context.Context, pool *pgxpool.Pool, schema, table string,
	opts Options) (lock *dbconn.TableLock, refused bool, err error) {
	lock, err = dbconn.AcquireTableLock(ctx, pool, schema, table, dbconn.TableLockOptions{Logger: opts.Logger})
	if errors.Is(err, dbconn.ErrTableLocked) {
		return nil, true, nil
	}
	if errors.Is(err, dbconn.ErrNoSessionAffinity) {
		return nil, false, fmt.Errorf("%s.%s cannot be changed through this connection: %w; %s",
			schema, table, err, sessionEndpointRemedy)
	}
	if err != nil {
		return nil, false, fmt.Errorf("take the change lock for %s.%s: %w", schema, table, err)
	}
	return lock, false, nil
}

// sessionEndpointRemedy names what an operator does about a connection that
// cannot hold a session-scoped lock. It is the same remedy on every hosted
// platform: the pooled port is the default in their setup docs, and the
// direct one is what an engine that takes locks needs.
const sessionEndpointRemedy = "point pg-sprite at the direct session endpoint rather than a transaction-mode pooler, " +
	"or run the pooler in session pool mode"

// releaseTableLock releases the lock and reports a disagreement about
// ownership. The change is over by the time this runs, so a lost lock cannot
// be acted on any more — but it says the exclusion this run believed it had
// was already gone, which is the one thing an operator needs to know before
// trusting what the run reported.
func releaseTableLock(ctx context.Context, lock *dbconn.TableLock, logger *slog.Logger) {
	if err := lock.Release(ctx); err != nil {
		logger.Error("the table change lock was not held at release; another instance may have been changing this table at the same time",
			"schema", lock.Schema(), "table", lock.Table(), "backend_pid", lock.BackendPID(), "error", err)
	}
}

// tableLockedVerdict refuses a statement because another instance is already
// changing the table.
func tableLockedVerdict(st statement.Statement) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeRefused,
		Reason:    verdict.ReasonTableLocked,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: "another pg-sprite instance holds the change lock for this table; " +
			"nothing was executed — retry once it finishes",
	}
}

// tableLockedResult refuses a whole convergence because another instance is
// already changing the table.
func tableLockedResult(req DesiredRequest) DesiredResult {
	return DesiredResult{
		Outcome: verdict.OutcomeRefused,
		Reason:  verdict.ReasonTableLocked,
		Detail: fmt.Sprintf("another pg-sprite instance holds the change lock for %s.%s; "+
			"nothing was planned or executed — retry once it finishes", req.Schema, req.Desired.Table()),
	}
}
