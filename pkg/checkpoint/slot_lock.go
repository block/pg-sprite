package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
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
// interleave (ST-3). The lock is transaction-scoped on a connection of its
// own, dialled from cfg outside the store's pool and closed once fn
// returns, which lets the lock go: fn draws on the pool for its own work,
// and a lock that sat on a pool connection would hold the seat fn is
// waiting for, so as many claims at once as the pool has connections would
// each wait on the others. A run holds the lock while it makes the slot and
// commits the row that names it, in either order; the reaper holds it from
// its last read of the row until the slot is dropped. Whichever goes first,
// the other sees the outcome: a reaper that waited finds the row and keeps
// the slot, and a run that waited finds whatever bore the name gone and
// creates afresh.
//
// The lock is let go however fn ends — a return, a panic the caller
// recovers, or the goroutine exiting — since the connection is closed on
// the way out. The lock is on the name, in the store's database: the same
// name in another database is another key, as D11 requires, so a cfg that
// dials another database or cluster than the store's pool is refused as
// ErrInvariantViolation before the lock is taken and fn runs. fn's error is
// returned as it is; a failure to take or let go of the lock is reported
// alongside.
func (s *Store) WithSlotLock(ctx context.Context, cfg dbconn.Config, name string, fn func(ctx context.Context) error) (err error) {
	conn, err := dbconn.ConnectSession(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect for the lock on slot %s: %w", name, err)
	}
	defer func() {
		err = errors.Join(err, releaseSlotLock(ctx, conn, name))
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the lock on slot %s: %w", name, err)
	}
	if err := s.proveSameDatabase(ctx, tx, name); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, slotLockSQL, slotLockClassID, name); err != nil {
		return fmt.Errorf("lock slot %s: %w", name, err)
	}
	return fn(ctx)
}

// databaseIdentitySQL names the cluster and the database a session is on.
const databaseIdentitySQL = `
	SELECT s.system_identifier::text, pg_catalog.current_database()
	FROM pg_catalog.pg_control_system() s`

// proveSameDatabase refuses a lock connection that is not on the store's
// cluster and database: the advisory key is per database, so a lock taken
// anywhere else would not exclude a claim or a reap judged through the
// store (ST-3, D11).
func (s *Store) proveSameDatabase(ctx context.Context, lock pgx.Tx, name string) error {
	var lockSystem, lockDatabase, storeSystem, storeDatabase string
	if err := lock.QueryRow(ctx, databaseIdentitySQL).Scan(&lockSystem, &lockDatabase); err != nil {
		return fmt.Errorf("identify the lock connection for slot %s: %w", name, err)
	}
	if err := s.pool.QueryRow(ctx, databaseIdentitySQL).Scan(&storeSystem, &storeDatabase); err != nil {
		return fmt.Errorf("identify the store's database for the lock on slot %s: %w", name, err)
	}
	if lockSystem != storeSystem {
		return fmt.Errorf("%w: ST-3: the lock on slot %s is on cluster %s, the store on cluster %s",
			ErrInvariantViolation, name, lockSystem, storeSystem)
	}
	if lockDatabase != storeDatabase {
		return fmt.Errorf("%w: ST-3: the lock on slot %s is on database %q, the store on %q",
			ErrInvariantViolation, name, lockDatabase, storeDatabase)
	}
	return nil
}

// releaseSlotLock closes the connection whose transaction holds the lock,
// even when ctx has ended; the server ends the transaction with the
// session, and the lock with it.
func releaseSlotLock(ctx context.Context, conn *pgx.Conn, name string) error {
	if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("release the lock on slot %s: %w", name, err)
	}
	return nil
}
