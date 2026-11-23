package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// runUnderParentLock runs body under the advisory table lock of the
// partitioned table schema.table, held on a session dialed from cfg, and
// releases the lock once body returns. purpose names the run in the error of a
// lock that could not be taken. body is not run when the lock is not taken or
// is not held in the pool's database, and the error it returns is replaced by
// an invariant violation when the lock was lost while it ran.
func runUnderParentLock(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, schema, table, purpose string, body func(ctx context.Context) error) error {
	// INV: LK-1 — the parent's advisory lock excludes a second run on the same
	// parent for the whole build.
	lock, err := dbconn.AcquireTableLock(ctx, cfg, schema, table)
	if err != nil {
		return fmt.Errorf("lock %s for %s: %w", qualifiedName(schema, table), purpose, err)
	}
	// INV: LK-1 — an advisory lock excludes only inside its own database, and
	// the steps run on the pool, so the lock must be held in the pool's
	// database before any step runs.
	if err := confirmLockInPoolDatabase(ctx, pool, lock, schema, table); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		if releaseErr := lock.Release(releaseCtx); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release the table lock for %s: %w", qualifiedName(schema, table), releaseErr))
		}
		return err
	}
	// Everything the lock protects runs under a context that ends when the
	// lock is lost, so no statement outlives the exclusion it relied on.
	lockedCtx, stop := lock.Bind(ctx)
	err = body(lockedCtx)
	stop()
	// INV: LK-1 — a lock lost while the build ran voids what it did under the
	// exclusion, whatever else it reported.
	if lost := lock.Err(); lost != nil {
		err = lockLostError(lost, err)
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
	defer cancel()
	if releaseErr := lock.Release(releaseCtx); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("release the table lock for %s: %w", qualifiedName(schema, table), releaseErr))
	}
	return err
}

// lockLostError is the error of a build whose table lock was lost. The step
// that was running when the lock went usually failed too, because the lost
// lock cancels its context, and its typed error (an invalid index its
// cancelled build left, an expired budget) would decide the outcome code
// ahead of the invariant violation. The step's error is therefore kept as
// text for the operator and not as a cause, so the code is always
// invariant-violation and errors.Is finds dbconn.ErrTableLockLost.
func lockLostError(lost, stepErr error) error {
	lockErr := fmt.Errorf("%w (LK-1): %w: %w", ErrInvariantViolation, dbconn.ErrTableLockLost, lost)
	if stepErr == nil {
		return lockErr
	}
	return fmt.Errorf("%w; the step that was running then failed: %s", lockErr, stepErr.Error())
}

// confirmLockInPoolDatabase proves, from a pool session, that the backend
// holding the build's table lock holds it in the database the pool is
// connected to. The lock is taken on a session dialed from cfg and every step
// runs on the pool, so a cfg and a pool that reach different databases would
// give the build no exclusion while both looked healthy.
func confirmLockInPoolDatabase(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, schema, table string) error {
	holder, found, err := dbconn.LookupTableLockHolder(ctx, pool, schema, table)
	if err != nil {
		return fmt.Errorf("confirm the table lock for %s from the pool: %w", qualifiedName(schema, table), err)
	}
	// A PID is unique only within one server, so the holder's backend start
	// must match too: the pair is what identifies the lock's own session.
	if !found || holder.PID != lock.BackendPID() || !holder.BackendStart.Equal(lock.BackendStart()) {
		return fmt.Errorf("%w: LK-1: the table lock for %s is not held in the database the pool is connected to; the connection target and the pool must reach the same database",
			ErrInvariantViolation, qualifiedName(schema, table))
	}
	return nil
}
