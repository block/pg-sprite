package decode

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// ErrSlotActive reports a slot DropIdleSlot found held by a walsender or a
// backend at the moment of the drop: whoever holds it is the judge of it,
// so the slot and its publication are left as found.
var ErrSlotActive = errors.New("replication slot is held by another process")

// ErrSlotDroppedPublicationLeft reports a drop that got as far as the slot
// and no further: no slot of the name remains — the drop removed it, or it
// was already absent — but the publication of the name was not dropped, so
// a caller that reports what it removed counts the slot as gone. It is
// joined with the publication drop's own error, a *ForeignStateError for a
// publication that is not the route's, so a caller routing on that error
// still sees it.
var ErrSlotDroppedPublicationLeft = errors.New("replication slot dropped, publication of its name left in place")

// DropSlot drops the engine's replication slot and publication of the given
// name from pool's database. The slot is dropped with DROP_REPLICATION_SLOT
// … WAIT on a fresh replication connection built from cfg, proven to be on
// the pool's own cluster and database before the drop is issued, so a
// walsender still holding the slot is waited for rather than failed on; ctx
// is the only bound on that wait, and a wait that ctx ends is cancelled on
// the server and reported as an error carrying ctx's own — never as a drop,
// and the slot stays for the holder to release and the next attempt to
// drop. The cancel request travels on a connection of its own, so a caller
// that must know what the server did re-reads InspectSlot. A drop is
// reported only once the catalog no longer shows the slot. A slot already absent is success, so the drop is
// idempotent.
//
// Only the engine's own state is dropped: a slot of the name that belongs to
// another database or is physical, and a publication of the name that is not
// one CreateSlot would have created, are refused with a *ForeignStateError
// and left as found — a foreign slot leaves the publication of the name in
// place too, since the whole name is then in dispute. The slot is dropped
// before the publication is judged, so a publication refused, or a
// publication drop that fails, after the slot is gone comes back as
// ErrSlotDroppedPublicationLeft joined with the publication's error: the
// slot is dropped and the publication stays. A name outside the
// engine's shape is an invariant violation. The reads that judge the slot,
// the server, and the publication do not consult pool's search_path, so a
// caller need not pin it. The slot is judged by the read InspectSlot makes,
// so pool's role needs what that read needs, EXECUTE on pg_is_in_recovery(),
// pg_current_wal_lsn() and pg_wal_lsn_diff() (PUBLIC holds it by default),
// and a read that fails refuses the drop.
func DropSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string) error {
	return dropSlotAndPublication(ctx, cfg, pool, name, true)
}

// DropIdleSlot is DropSlot for a caller that must not wait out a holder: the
// slot is dropped with DROP_REPLICATION_SLOT without WAIT, so a slot a
// walsender or a backend holds when the drop reaches the server is refused
// with ErrSlotActive and left in place, publication and all. Whether the
// slot is held is judged by the server at the drop itself, not by a catalog
// read before it, so a slot that becomes held between a caller's read and
// the drop is still left alone. Everything else is as for DropSlot,
// ErrSlotDroppedPublicationLeft for a publication left after the slot's
// drop included.
func DropIdleSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string) error {
	return dropSlotAndPublication(ctx, cfg, pool, name, false)
}

// dropSlotAndPublication is the drop behind DropSlot and DropIdleSlot: the
// slot, then the publication of the name. A slot drop that fails or is
// refused leaves the publication in place; a publication drop that fails or
// is refused once the slot is gone is ErrSlotDroppedPublicationLeft, so the
// caller learns the slot went.
func dropSlotAndPublication(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string, wait bool) error {
	if err := dropSlot(ctx, cfg, pool, name, wait); err != nil {
		return err
	}
	if err := dropOwnPublication(ctx, pool, name); err != nil {
		return fmt.Errorf("slot %s: %w: %w", name, ErrSlotDroppedPublicationLeft, err)
	}
	return nil
}

// dropSlot drops the engine's slot of the name and nothing else; wait says
// whether the server is to wait for a holder or refuse with ErrSlotActive.
// A slot already absent is success.
func dropSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, name string, wait bool) error {
	if !slotNamePattern.MatchString(name) {
		return fmt.Errorf("%w: ST-3: %q is not an engine slot name", ErrInvariantViolation, name)
	}
	// INV: ST-3 — the slot is judged the engine's by InspectSlot's Own, the
	// one statement that spells "a logical slot of the pool's own
	// database"; it never consults the pool's search_path (CO-9), and a
	// read that fails or cannot be parsed refuses the drop.
	status, found, err := InspectSlot(ctx, pool, name)
	if err != nil {
		return fmt.Errorf("drop slot %s: %w", name, err)
	}
	if !found {
		return nil
	}
	if !status.Own {
		return foreignSlot(name)
	}
	if err := dropReplicationSlot(ctx, cfg, pool, name, wait); err != nil {
		return err
	}
	return confirmSlotGone(ctx, pool, name)
}

// confirmSlotGone re-reads the catalog after a drop the server accepted and
// refuses to report a drop while the slot is still there.
func confirmSlotGone(ctx context.Context, pool *pgxpool.Pool, name string) error {
	_, found, err := InspectSlot(ctx, pool, name)
	if err != nil {
		return fmt.Errorf("confirm drop of slot %s: %w", name, err)
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
	conn, err := dbconn.ConnectReplication(ctx, cfg, dbconn.WithServerSideCancel(slotCommandCancelGrace))
	if err != nil {
		return fmt.Errorf("drop slot %s: %w", name, err)
	}
	if _, err := proveSameServer(ctx, conn, pool); err != nil {
		return errors.Join(fmt.Errorf("drop slot %s: %w", name, err), conn.Close(context.WithoutCancel(ctx)))
	}
	// INV: ST-3 — a wait for the holder is bounded by ctx alone; ending it
	// cancels the drop on the server and is an error to the caller, not a
	// dropped slot.
	err = endedByContext(ctx, pglogrepl.DropReplicationSlot(ctx, conn, name, pglogrepl.DropReplicationSlotOptions{Wait: wait}))
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
