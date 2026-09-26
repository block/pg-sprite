package dbconn

import (
	"context"
	"errors"
	"fmt"
)

// ErrTableLockNotHeld reports that no session holds a table's advisory lock
// at the moment a working transaction asked the server to confirm it. It is
// always wrapped in ErrInvariantViolation: the lock session believed it held
// the table.
var ErrTableLockNotHeld = errors.New("no session holds the table lock")

// Confirm re-asserts the session's table lock from inside a working
// transaction on another connection, before that transaction's first write:
// pg_locks, read on the connection about to write, must show the lock granted
// to this session's own backend. The lock and the work are deliberately on
// different sessions, so this is the one point where the server confirms that
// the session a transaction trusts is the session that actually holds the
// table.
//
// A missing lock is ErrInvariantViolation wrapping ErrTableLockNotHeld; a
// lock granted to a different backend is a *TableLockHeldError naming it.
func (s *TableLockSession) Confirm(ctx context.Context, conn AdvisoryLockHolder) error {
	name := tableLockQualifiedName(s.lock.Schema(), s.lock.Table())
	holder, found, err := lookupTableLockHolder(ctx, conn, s.lock.Key())
	if err != nil {
		return fmt.Errorf("confirm table lock on %s: %w", name, err)
	}
	// INV: LK-1
	if !found {
		return fmt.Errorf("%w: LK-1: %w on %s", ErrInvariantViolation, ErrTableLockNotHeld, name)
	}
	if holder.PID != s.pid {
		return &TableLockHeldError{Schema: s.lock.Schema(), Table: s.lock.Table(), Holder: holder}
	}
	return nil
}
