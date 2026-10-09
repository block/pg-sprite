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

// ErrSlotActive reports a slot DropIdleSlot found held by a walsender or a
// backend at the moment of the drop: whoever holds it is the judge of it,
// so the slot and its publication are left as found.
var ErrSlotActive = errors.New("replication slot is held by another process")

// DropSlot drops the engine's replication slot and publication of the given
// name from pool's database. The slot is dropped with DROP_REPLICATION_SLOT
// … WAIT on a fresh replication connection built from cfg, proven to be on
// the pool's own cluster and database before the drop is issued, so a
// walsender still holding the slot is waited for rather than failed on; ctx
// is the only bound on that wait, and a wait that ctx ends is an error —
// never a report that the slot is gone, though the server may still complete
// the drop once the holder releases the slot, so a caller that needs to know
// re-reads InspectSlot. A drop is reported only once the catalog no longer
// shows the slot. A slot already absent is success, so the drop is
// idempotent.
//
// Only the engine's own state is dropped: a slot of the name that belongs to
// another database or is physical, and a publication of the name that is not
// one CreateSlot would have created, are refused with a *ForeignStateError
// and left as found — a foreign slot leaves the publication of the name in
// place too, since the whole name is then in dispute. A name outside the
// engine's shape is an invariant violation.
func DropSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string) error {
	return dropSlot(ctx, cfg, pool, name, true)
}

// DropIdleSlot is DropSlot for a caller that must not wait out a holder: the
// slot is dropped with DROP_REPLICATION_SLOT without WAIT, so a slot a
// walsender or a backend holds when the drop reaches the server is refused
// with ErrSlotActive and left in place, publication and all. Whether the
// slot is held is judged by the server at the drop itself, not by a catalog
// read before it, so a slot that becomes held between a caller's read and
// the drop is still left alone. Everything else is as for DropSlot.
func DropIdleSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string) error {
	return dropSlot(ctx, cfg, pool, name, false)
}

// dropSlot is the drop behind DropSlot and DropIdleSlot; wait says whether
// the server is to wait for a holder or refuse with ErrSlotActive.
func dropSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string, wait bool) error {
	if !slotNamePattern.MatchString(name) {
		return fmt.Errorf("%w: ST-3: %q is not an engine slot name", ErrInvariantViolation, name)
	}
	own, found, err := slotIsOwn(ctx, pool, name)
	if err != nil {
		return err
	}
	if found && !own {
		return foreignSlot(name)
	}
	if found {
		if err := dropReplicationSlot(ctx, cfg, pool, name, wait); err != nil {
			return err
		}
		if err := confirmSlotGone(ctx, pool, name); err != nil {
			return err
		}
	}
	return dropOwnPublication(ctx, pool, name)
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

// confirmSlotGone re-reads the catalog after a drop the server accepted and
// refuses to report a drop while the slot is still there.
func confirmSlotGone(ctx context.Context, pool *pgxpool.Pool, name string) error {
	_, found, err := slotIsOwn(ctx, pool, name)
	if err != nil {
		return err
	}
	// INV: ST-3 — success means the catalog no longer shows the slot.
	if found {
		return fmt.Errorf("%w: ST-3: slot %s is still present after its drop was accepted", ErrInvariantViolation, name)
	}
	return nil
}

// dropReplicationSlot issues the drop on its own replication connection,
// proven to be on pool's server first, and closes it afterwards whatever
// the outcome. With wait the server waits for a holder; without it a held
// slot is ErrSlotActive.
func dropReplicationSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string, wait bool) error {
	conn, err := dbconn.ConnectReplication(ctx, cfg)
	if err != nil {
		return fmt.Errorf("drop slot %s: %w", name, err)
	}
	if _, err := proveSameServer(ctx, conn, pool); err != nil {
		return errors.Join(fmt.Errorf("drop slot %s: %w", name, err), conn.Close(context.WithoutCancel(ctx)))
	}
	// INV: ST-3 — a wait for the holder is bounded by ctx alone; ending it
	// is an error to the caller, not a dropped slot.
	err = pglogrepl.DropReplicationSlot(ctx, conn, name, pglogrepl.DropReplicationSlotOptions{Wait: wait})
	var closeErr error
	if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
		closeErr = fmt.Errorf("close replication connection for slot %s: %w", name, err)
	}
	switch {
	case err == nil:
		return closeErr
	case isSQLState(err, sqlstateUndefinedObject):
		// The slot went away between the catalog read and the drop on the
		// server the proof tied the connection to; the outcome the caller
		// asked for holds.
		return closeErr
	case !wait && isSQLState(err, sqlstateObjectInUse):
		// INV: ST-3 — a slot held at the drop is its holder's; the server's
		// verdict at the drop itself outranks any catalog read before it.
		return errors.Join(fmt.Errorf("drop slot %s: %w: %w", name, ErrSlotActive, err), closeErr)
	default:
		return errors.Join(fmt.Errorf("drop slot %s: %w", name, err), closeErr)
	}
}
