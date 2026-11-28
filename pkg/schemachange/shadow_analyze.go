package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// ShadowStatistics is the outcome of the ANALYZE a run makes of its
// shadow before the swap. A shadow is a new relation with no planner
// statistics of its own, and the swap moves its OID under the source's
// name, so whatever ANALYZE leaves in pg_class.reltuples, pg_statistic,
// and pg_statistic_ext_data for the shadow is what the planner finds for
// the table the moment the swap commits. Without it the table carries no
// statistics until autovacuum's change threshold trips, which on a table
// with autovacuum off is never.
type ShadowStatistics struct {
	// Analyzed reports that the ANALYZE committed.
	Analyzed bool
	// Err is why it did not; nil when Analyzed.
	Err error
}

// analyzeShadowStatistics runs analyzeShadow and records its outcome. A
// failed ANALYZE does not stop the run: the shadow's rows are verified and
// the swap is as correct without statistics as with them, so the outcome
// rides on Result for the caller to report. A failure the run cannot go on
// from — a lost table lock, a cancelled context — stops the next step,
// which meets the same cause.
func (r *Runner) analyzeShadowStatistics(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, state *runState) {
	if err := analyzeShadow(ctx, pool, lock, state.built, r.opts.Shadow); err != nil {
		state.statistics = &ShadowStatistics{Err: err}
		return
	}
	state.statistics = &ShadowStatistics{Analyzed: true}
}

// analyzeShadow runs a whole-table ANALYZE of the built shadow, in one
// transaction under the shadow options' budgets, as the owner, and only
// while the caller holds the table lock. The whole table is analyzed so
// expression indexes and extended statistics objects, which the server
// computes only from a whole-table sample, are covered too; the per-column
// and extended statistics targets the build carried over are already on
// the shadow, so the sample is sized as the source's would be.
//
// ANALYZE takes SHARE UPDATE EXCLUSIVE on the shadow alone, which the
// copier's and the applier's ACCESS SHARE and ROW EXCLUSIVE do not
// conflict with, and touches nothing of the source, so a live run's
// catch-up keeps applying the stream and a quiesced run's write fence
// stays as it was. The lock is taken explicitly first, under the
// transaction's lock_timeout, so the OID read after it is the relation
// the ANALYZE then samples: once locked, the shadow cannot be renamed or
// replaced under its name.
func analyzeShadow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, built BuiltShadow, opts Options) error {
	ctx, stop := lock.Bind(ctx)
	defer stop()
	schema, shadow := built.Schema(), built.ShadowTable()
	name := pgx.Identifier{schema, shadow}.Sanitize()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin analyze of shadow %s.%s: %w", schema, shadow, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	// INV: LK-2 — the ANALYZE waits on a conflicting lock no longer than
	// the lock budget and runs no longer than the statement budget.
	if err := setGateSession(ctx, tx, built, opts); err != nil {
		return err
	}
	// INV: LK-1
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return lockLossCause(lock, err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+name+" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock shadow %s.%s for analyze: %w", schema, shadow, err)
	}
	oid, err := resolveRelation(ctx, tx, schema, shadow)
	if err != nil {
		return err
	}
	// INV: ST-6
	if oid != built.ShadowOID() {
		return refuse(CauseRelationReplaced, nil, "shadow %s.%s is relation %d, the build proved %d", schema, shadow, oid, built.ShadowOID())
	}
	if _, err := tx.Exec(ctx, "ANALYZE "+name); err != nil {
		return fmt.Errorf("analyze shadow %s.%s: %w", schema, shadow, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit analyze of shadow %s.%s: %w", schema, shadow, err)
	}
	return nil
}
