package decode

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// DropSlot drops the engine's replication slot and publication of the given
// name from pool's database. The slot is dropped with DROP_REPLICATION_SLOT
// … WAIT on a fresh replication connection built from cfg, so a walsender
// still holding the slot is waited for rather than failed on; ctx is the
// only bound on that wait, and a wait that ctx ends is an error — never a
// report that the slot is gone, though the server may still complete the
// drop once the holder releases the slot, so a caller that needs to know
// re-reads InspectSlot. A slot already absent is success, so the drop is
// idempotent.
//
// Only a logical slot of pool's own database is dropped: a slot of the name
// that belongs to another database, or a physical slot, is refused with
// ErrForeignDecodingState, and a name outside the engine's shape is an
// invariant violation — the primitive cannot be pointed at anyone else's
// slot.
func DropSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string) error {
	if !slotNamePattern.MatchString(name) {
		return fmt.Errorf("%w: ST-3: %q is not an engine slot name", ErrInvariantViolation, name)
	}
	own, found, err := slotIsOwn(ctx, pool, name)
	if err != nil {
		return err
	}
	if found && !own {
		return fmt.Errorf("%w: slot %s belongs to another database or is physical", ErrForeignDecodingState, name)
	}
	if found {
		if err := dropReplicationSlot(ctx, cfg, name); err != nil {
			return err
		}
	}
	if _, err := pool.Exec(ctx, `DROP PUBLICATION IF EXISTS `+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("drop publication %s: %w", name, err)
	}
	return nil
}

// slotIsOwn reports whether the named slot exists and, if so, whether it is
// a logical slot of pool's own database.
func slotIsOwn(ctx context.Context, pool *pgxpool.Pool, name string) (own, found bool, err error) {
	err = pool.QueryRow(ctx, `
		SELECT s.slot_type = 'logical' AND s.database = pg_catalog.current_database()
		FROM pg_catalog.pg_replication_slots s
		WHERE s.slot_name = $1`, name).Scan(&own)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read slot %s: %w", name, err)
	}
	return own, true, nil
}

// dropReplicationSlot issues the waiting drop on its own replication
// connection and closes it afterwards whatever the outcome.
func dropReplicationSlot(ctx context.Context, cfg dbconn.Config, name string) error {
	conn, err := dbconn.ConnectReplication(ctx, cfg)
	if err != nil {
		return fmt.Errorf("drop slot %s: %w", name, err)
	}
	// INV: ST-3 — the wait is bounded by ctx alone; ending it is an error
	// to the caller, not a dropped slot.
	err = pglogrepl.DropReplicationSlot(ctx, conn, name, pglogrepl.DropReplicationSlotOptions{Wait: true})
	var closeErr error
	if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
		closeErr = fmt.Errorf("close replication connection for slot %s: %w", name, err)
	}
	switch {
	case err == nil:
		return closeErr
	case isSQLState(err, sqlstateUndefinedObject):
		// The slot went away between the catalog read and the drop; the
		// outcome the caller asked for holds.
		return closeErr
	default:
		return errors.Join(fmt.Errorf("drop slot %s: %w", name, err), closeErr)
	}
}
