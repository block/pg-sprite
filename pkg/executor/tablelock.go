package executor

import (
	"context"
	"fmt"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// guardTableLock re-verifies the LK-1 proof at the point of use and returns
// the context the mutating work must run under: one that is cancelled the
// moment the lock stops being held.
//
// A zero proof is forgeable by any package — only dbconn.AcquireTableLock
// mints a real one — and a proof minted an hour ago says nothing about now,
// so the confirmation reads the catalog on the lock's own session rather
// than trusting the value in hand. The caller must call the returned cancel
// function.
//
// INV: LK-1 — at most one schema change runs per table, and losing the lock
// is fail-closed.
func guardTableLock(ctx context.Context, lock *dbconn.TableLock) (context.Context, context.CancelFunc, error) {
	if lock == nil {
		return nil, nil, fmt.Errorf("%w: LK-1: no table lock proof was supplied", ErrInvariantViolation)
	}
	guarded, cancel, err := lock.Guard(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: LK-1: the table lock for %s is not held: %w",
			ErrInvariantViolation, qualifiedName(lock.Schema(), lock.Table()), err)
	}
	return guarded, cancel, nil
}

// guardTableLockFor is guardTableLock for a mutating operation with a known
// target: it first refuses a proof taken on a different table, so a lock for
// one table cannot license a change to another.
func guardTableLockFor(ctx context.Context, lock *dbconn.TableLock, schema, table string) (context.Context, context.CancelFunc, error) {
	if lock == nil {
		return nil, nil, fmt.Errorf("%w: LK-1: no table lock proof was supplied for %s",
			ErrInvariantViolation, qualifiedName(schema, table))
	}
	if lock.Schema() != schema || lock.Table() != table {
		return nil, nil, fmt.Errorf("%w: LK-1: the table lock is held on %s but the change targets %s",
			ErrInvariantViolation, qualifiedName(lock.Schema(), lock.Table()), qualifiedName(schema, table))
	}
	return guardTableLock(ctx, lock)
}
