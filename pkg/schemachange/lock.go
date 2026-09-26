package schemachange

import (
	"context"
	"errors"

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
		return refuse(CauseLockUnproven, nil, "shadow operations require a table lock session")
	}
	held := lock.Lock()
	if held.Table() == "" {
		return refuse(CauseLockUnproven, nil, "table lock proof is empty")
	}
	if held.Schema() != target.Schema() || held.Table() != target.Table() {
		return refuse(CauseLockUnproven, nil, "table lock is for %s.%s, proof is for %s.%s", held.Schema(), held.Table(), target.Schema(), target.Table())
	}
	if err := lock.Err(); err != nil {
		return refuse(CauseLockLost, []error{err}, "table lock was lost before the shadow operation")
	}
	return nil
}

// confirmTableLock re-asserts the lock from inside the working transaction,
// before its first write, and names the refusal cause behind each way the
// server can disagree with the lock session about who holds the table.
func confirmTableLock(ctx context.Context, tx pgx.Tx, lock *dbconn.TableLockSession) error {
	err := lock.Confirm(ctx, tx)
	if err == nil {
		return nil
	}
	held := lock.Lock()
	// INV: LK-1
	if errors.Is(err, dbconn.ErrTableLockNotHeld) {
		return refuse(CauseLockUnconfirmed, []error{err}, "no session holds the table lock on %s.%s", held.Schema(), held.Table())
	}
	var heldElsewhere *dbconn.TableLockHeldError
	if errors.As(err, &heldElsewhere) {
		return refuse(CauseLockHeldElsewhere, []error{err}, "table lock on %s.%s is held by backend %d, not the lock session's backend %d", held.Schema(), held.Table(), heldElsewhere.Holder.PID, lock.BackendPID())
	}
	return err
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
	return refuse(CauseLockLost, []error{lost, err}, "table lock was lost during the shadow operation")
}
