package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// Reaped is what one pass of ReapOrphanSlots found among the engine slots
// of the store's database, every slot in exactly one list.
type Reaped struct {
	// Dropped slots had no checkpoint row, or a terminal one, and no holder:
	// orphans, dropped with their publications.
	Dropped []string
	// Live slots are named by a checkpoint row in a non-terminal phase: a
	// schema change in flight or resumable owns each.
	Live []string
	// Active slots were held by a walsender or a backend when the reaper
	// went to drop them; whoever holds one is the judge of it, so the
	// reaper leaves it for a later pass.
	Active []string
	// Foreign slots wear the engine's prefix without its shape; they are
	// somebody else's and are reported, never dropped.
	Foreign []string
}

// candidateSlotsSQL lists the slots of the store's database that wear the
// engine's prefix. Slots of other databases are never read: a slot is
// cluster state, but the row that owns it is per database, so only this
// database's rows can speak for this database's slots (D11). The active
// flag is advisory: pg_replication_slots reads shared memory, not a
// snapshot, so holders come and go underneath any statement, and the drop
// itself is what finds out whether a slot is held.
const candidateSlotsSQL = `
SELECT slot_name, active
FROM pg_catalog.pg_replication_slots
WHERE database = pg_catalog.current_database()
  AND slot_type = 'logical'
  AND slot_name LIKE $1
ORDER BY slot_name`

// liveRowSlotsSQL names, among the given slots, those a checkpoint row in a
// non-terminal phase owns.
const liveRowSlotsSQL = `
SELECT slot_name
FROM ` + tableIdent + `
WHERE slot_name = ANY($1)
  AND phase NOT IN ($2, $3)`

// candidateSlot is one row of candidateSlotsSQL.
type candidateSlot struct {
	name   string
	active bool
}

// ReapOrphanSlots drops the engine's orphan replication slots in the store's
// database (ST-3). An orphan is a logical slot of the engine's name shape
// that no checkpoint row in a non-terminal phase names and that no process
// holds: a run writes and commits its row before it creates its slot, so a
// slot with no row was left by a run that ended without dropping it, and a
// slot named by a done or failed row by one whose cleanup did not finish.
//
// Ownership is judged twice. The slots are listed first and the rows that
// own them read in a later statement, so a row committed before its slot
// appears is seen whenever the slot is; then, right before each drop, the
// row is read again, so a run that claimed the slot since the pass began
// keeps it. Whether a slot is held is judged at the drop: each orphan goes
// through decode.DropIdleSlot on a replication connection built from cfg,
// which refuses a held slot instead of waiting for its holder and takes an
// idle slot's publication with it. Slots a live row names or a process
// holds, and names that wear the prefix without the shape, are reported
// and left as found. The drops stop at the first failure, reporting what
// was dropped before it.
//
// Two reapers over one database race each other at the drop; until a lock
// coordinates them, run one reaper per database at a time.
//
// A database where Ensure has never run has no rows to speak for its
// slots, so the pass is refused with ErrTableMissing rather than read every
// slot as an orphan.
func (s *Store) ReapOrphanSlots(ctx context.Context, cfg dbconn.Config) (Reaped, error) {
	candidates, err := s.candidateSlots(ctx)
	if err != nil {
		return Reaped{}, err
	}
	live, err := s.liveRowSlots(ctx, slotNames(candidates))
	if err != nil {
		return Reaped{}, err
	}
	var reaped Reaped
	for _, c := range candidates {
		switch {
		case !decode.IsEngineSlotName(c.name):
			reaped.Foreign = append(reaped.Foreign, c.name)
		case live[c.name]:
			// INV: ST-3 — a row in flight owns its slot; the reaper never
			// drops a slot a schema change can still resume on.
			reaped.Live = append(reaped.Live, c.name)
		case c.active:
			reaped.Active = append(reaped.Active, c.name)
		default:
			if err := s.dropIfStillOrphan(ctx, cfg, c.name, &reaped); err != nil {
				return reaped, err
			}
		}
	}
	return reaped, nil
}

// dropIfStillOrphan re-reads the slot's row and then drops the slot without
// waiting for a holder, filing the slot under the list the outcome names.
func (s *Store) dropIfStillOrphan(ctx context.Context, cfg dbconn.Config, name string, reaped *Reaped) error {
	live, err := s.liveRowSlots(ctx, []string{name})
	if err != nil {
		return err
	}
	if live[name] {
		// INV: ST-3 — the row is read right before the drop so a run that
		// claimed the slot since the pass began keeps it.
		reaped.Live = append(reaped.Live, name)
		return nil
	}
	err = decode.DropIdleSlot(ctx, cfg, s.pool, name)
	if errors.Is(err, decode.ErrSlotActive) {
		reaped.Active = append(reaped.Active, name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reap orphan slot %s: %w", name, err)
	}
	reaped.Dropped = append(reaped.Dropped, name)
	return nil
}

// candidateSlots reads the engine-prefixed slots of the store's database.
func (s *Store) candidateSlots(ctx context.Context) ([]candidateSlot, error) {
	rows, err := s.pool.Query(ctx, candidateSlotsSQL, likePrefix(decode.SlotNamePrefix))
	if err != nil {
		return nil, fmt.Errorf("list engine slots: %w", err)
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidateSlot, error) {
		var c candidateSlot
		err := row.Scan(&c.name, &c.active)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("list engine slots: %w", err)
	}
	return candidates, nil
}

// liveRowSlots reads which of the named slots a checkpoint row in a
// non-terminal phase owns. It is asked even when there are no names, so a
// database without the checkpoint table is refused whether or not it has
// slots.
func (s *Store) liveRowSlots(ctx context.Context, names []string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, liveRowSlotsSQL, names, PhaseDone.String(), PhaseFailed.String())
	if err != nil {
		return nil, fmt.Errorf("read slot owners: %w", missingTable(err))
	}
	owned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read slot owners: %w", missingTable(err))
	}
	live := make(map[string]bool, len(owned))
	for _, name := range owned {
		live[name] = true
	}
	return live, nil
}

// slotNames lists the candidates' names in order.
func slotNames(candidates []candidateSlot) []string {
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.name)
	}
	return names
}

// likePrefix is the LIKE pattern matching names that start with prefix,
// with LIKE's own wildcards in prefix escaped so an underscore matches an
// underscore.
func likePrefix(prefix string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return escaped + "%"
}
