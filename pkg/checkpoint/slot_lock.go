package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// slotLockClassID is the high half of the advisory key the engine takes on
// a replication slot's name: "pgsl" in ASCII, distinct from the table
// lock's class so a slot lock and a table lock whose low halves collide
// are still two keys.
const slotLockClassID int32 = 0x7067736c

// slotLockSQL takes the engine's advisory lock on a slot name for the rest
// of the transaction. The wait is bounded by the session's lock_timeout
// (LK-2), so a holder that does not let go in time is reported as the
// server's lock timeout, never waited out.
const slotLockSQL = "SELECT pg_catalog.pg_advisory_xact_lock($1, pg_catalog.hashtext($2))"

// WithSlotLock runs fn while this store holds the engine's advisory lock on
// the named slot, so a run claiming the slot and a reaper judging it never
// interleave (ST-3). The lock is transaction-scoped on one of the store's
// connections, held open and idle while fn runs on whatever connections fn
// uses; the transaction is rolled back once fn returns, which lets the
// lock go. A run holds it while it makes the slot and commits the row that
// names it, in either order; the reaper holds it from its last read of the
// row until the slot is dropped. Whichever goes first, the other sees the
// outcome: a reaper that waited finds the row and keeps the slot, and a
// run that waited finds whatever bore the name gone and creates afresh.
//
// The lock is on the name, in the store's database: the same name in
// another database is another key, as D11 requires. fn's error is
// returned as it is; a failure to take or let go of the lock is reported
// alongside.
func (s *Store) WithSlotLock(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the lock on slot %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, slotLockSQL, slotLockClassID, name); err != nil {
		return errors.Join(
			fmt.Errorf("lock slot %s: %w", name, err),
			rollbackSlotLock(ctx, tx, name))
	}
	return errors.Join(fn(ctx), rollbackSlotLock(ctx, tx, name))
}

// rollbackSlotLock ends the transaction that holds the lock, letting it go
// even when ctx has ended.
func rollbackSlotLock(ctx context.Context, tx pgx.Tx, name string) error {
	if err := tx.Rollback(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("release the lock on slot %s: %w", name, err)
	}
	return nil
}
