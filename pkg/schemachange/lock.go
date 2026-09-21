package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// requireTableLock refuses to touch a shadow without the per-table lock that
// keeps a second engine instance off the same table: the session must exist,
// carry a populated proof for the proven table, and not have reported loss.
// It runs before any connection is opened, so a refusal costs nothing.
func requireTableLock(lock *dbconn.TableLockSession, target preflight.CopySwapTarget) error {
	// INV: LK-1
	if lock == nil {
		return fmt.Errorf("%w: LK-1: shadow operations require a table lock session", ErrInvariantViolation)
	}
	held := lock.Lock()
	if held.Table() == "" {
		return fmt.Errorf("%w: LK-1: table lock proof is empty", ErrInvariantViolation)
	}
	if held.Schema() != target.Schema() || held.Table() != target.Table() {
		return fmt.Errorf("%w: LK-1: table lock is for %s.%s, proof is for %s.%s", ErrInvariantViolation, held.Schema(), held.Table(), target.Schema(), target.Table())
	}
	if err := lock.Err(); err != nil {
		return fmt.Errorf("%w: LK-1: table lock was lost: %w", ErrInvariantViolation, err)
	}
	return nil
}

// confirmTableLock re-asserts the lock from inside the working transaction,
// before its first write: pg_locks, read on the connection about to write,
// must show the lock granted to the lock session's own backend. The lock
// and the work are deliberately on different sessions, so this is the one
// point where the server confirms that the session this transaction trusts
// is the session that actually holds the table.
func confirmTableLock(ctx context.Context, tx pgx.Tx, lock *dbconn.TableLockSession) error {
	held := lock.Lock()
	holder, found, err := dbconn.LookupTableLockHolder(ctx, tx, held.Schema(), held.Table())
	if err != nil {
		return fmt.Errorf("confirm table lock on %s.%s: %w", held.Schema(), held.Table(), err)
	}
	// INV: LK-1
	if !found {
		return fmt.Errorf("%w: LK-1: no session holds the table lock on %s.%s", ErrInvariantViolation, held.Schema(), held.Table())
	}
	if holder.PID != lock.BackendPID() {
		return fmt.Errorf("%w: LK-1: table lock on %s.%s is held by backend %d, not the lock session's backend %d", ErrInvariantViolation, held.Schema(), held.Table(), holder.PID, lock.BackendPID())
	}
	return nil
}

// lockLossCause reports the table-lock loss behind a failed operation: when
// the session has recorded loss, the failure is an invariant violation
// naming that loss, whatever statement error the cancelled Bind context
// produced. A session that still holds the lock returns err unchanged, so a
// caller's own cancellation — with whatever cause it chose — is never
// mistaken for a lock loss that did not happen.
func lockLossCause(lock *dbconn.TableLockSession, err error) error {
	lost := lock.Err()
	if lost == nil {
		return err
	}
	// INV: LK-1
	return fmt.Errorf("%w: LK-1: table lock lost during shadow operation: %w (%w)", ErrInvariantViolation, lost, err)
}
