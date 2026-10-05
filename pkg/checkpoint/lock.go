package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// requireTableLock refuses a checkpoint write without the per-table lock
// session that keeps a second engine instance off the same target: the
// session must exist, be for schema.table, and not have reported loss. It
// runs before any connection is opened, so a refusal costs nothing.
func requireTableLock(lock *dbconn.TableLockSession, schema, table string) error {
	// INV: LK-1
	if lock == nil {
		return fmt.Errorf("%w (LK-1): checkpoint writes for %s.%s require a table lock session", ErrInvariantViolation, schema, table)
	}
	held := lock.Lock()
	if held.Schema() != schema || held.Table() != table {
		return fmt.Errorf("%w (LK-1): table lock is for %s.%s, checkpoint is for %s.%s", ErrInvariantViolation, held.Schema(), held.Table(), schema, table)
	}
	if err := lock.Err(); err != nil {
		return fmt.Errorf("%w (LK-1): table lock was lost before the checkpoint write: %w", ErrInvariantViolation, err)
	}
	return nil
}

// confirmTableLock asks the server, on the connection about to write,
// whether the lock session's own backend holds the target. A server that
// answers "no one" or "another backend" has contradicted the session the
// store trusts, and that is the store's invariant violation; a lookup that
// got no answer is the connection's error, reported as such.
func confirmTableLock(ctx context.Context, conn dbconn.AdvisoryLockHolder, lock *dbconn.TableLockSession) error {
	err := lock.Confirm(ctx, conn)
	if err == nil {
		return nil
	}
	if lockDenied(err) {
		// INV: LK-1
		return fmt.Errorf("%w (LK-1): %w", ErrInvariantViolation, err)
	}
	return fmt.Errorf("confirm the table lock from the checkpoint transaction: %w", err)
}

// lockDenied reports whether a Confirm error is the server's word that the
// session does not hold the table, in either of the two forms dbconn gives
// it.
func lockDenied(err error) bool {
	if errors.Is(err, dbconn.ErrTableLockNotHeld) {
		return true
	}
	var heldElsewhere *dbconn.TableLockHeldError
	return errors.As(err, &heldElsewhere)
}
