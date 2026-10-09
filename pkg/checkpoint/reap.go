package checkpoint

import (
	"context"
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
	// Active slots are held by a walsender or a backend right now; whoever
	// holds one is the judge of it, so the reaper leaves it for a later
	// pass.
	Active []string
	// Foreign slots wear the engine's prefix without its shape; they are
	// somebody else's and are reported, never dropped.
	Foreign []string
}

// candidateSlotsSQL lists the slots of the store's database that wear the
// engine's prefix, with whether a non-terminal checkpoint row names each.
// Slots of other databases are never read: a slot is cluster state, but
// the row that owns it is per database, so only this database's rows can
// speak for this database's slots (D11). The checkpoint table is read in
// the same statement, so a row saved before the slot it names is created
// is seen whenever the slot is.
const candidateSlotsSQL = `
SELECT s.slot_name, s.active, c.slot_name IS NOT NULL
FROM pg_catalog.pg_replication_slots s
LEFT JOIN ` + tableIdent + ` c
       ON c.slot_name = s.slot_name AND c.phase NOT IN ($1, $2)
WHERE s.database = pg_catalog.current_database()
  AND s.slot_type = 'logical'
  AND s.slot_name LIKE $3
ORDER BY s.slot_name`

// candidateSlot is one row of candidateSlotsSQL.
type candidateSlot struct {
	name   string
	active bool
	live   bool
}

// ReapOrphanSlots drops the engine's orphan replication slots in the store's
// database (ST-3). An orphan is a logical slot of the engine's name shape
// that no checkpoint row in a non-terminal phase names and that no process
// holds: a run writes and commits its row before it creates its slot, so a
// slot with no row was left by a run that ended without dropping it, and a
// slot named by a done or failed row by one whose cleanup did not finish.
// Each orphan is dropped through decode.DropSlot on a replication
// connection built from cfg, which takes its publication with it. Slots a
// live row names or a process holds, and names that wear the prefix without
// the shape, are reported and left as found. The drops stop at the first
// failure, reporting what was dropped before it.
//
// A database where Ensure has never run has no rows to speak for its
// slots, so the pass is refused with ErrTableMissing rather than read every
// slot as an orphan.
func (s *Store) ReapOrphanSlots(ctx context.Context, cfg dbconn.Config) (Reaped, error) {
	candidates, err := s.candidateSlots(ctx)
	if err != nil {
		return Reaped{}, err
	}
	var reaped Reaped
	for _, c := range candidates {
		switch {
		case !decode.IsEngineSlotName(c.name):
			reaped.Foreign = append(reaped.Foreign, c.name)
		case c.live:
			// INV: ST-3 — a row in flight owns its slot; the reaper never
			// drops a slot a schema change can still resume on.
			reaped.Live = append(reaped.Live, c.name)
		case c.active:
			reaped.Active = append(reaped.Active, c.name)
		default:
			if err := decode.DropSlot(ctx, cfg, s.pool, c.name); err != nil {
				return reaped, fmt.Errorf("reap orphan slot %s: %w", c.name, err)
			}
			reaped.Dropped = append(reaped.Dropped, c.name)
		}
	}
	return reaped, nil
}

// candidateSlots reads the engine-prefixed slots of the store's database
// alongside the rows that own them.
func (s *Store) candidateSlots(ctx context.Context) ([]candidateSlot, error) {
	rows, err := s.pool.Query(ctx, candidateSlotsSQL,
		PhaseDone.String(), PhaseFailed.String(), likePrefix(decode.SlotNamePrefix))
	if err != nil {
		return nil, fmt.Errorf("list engine slots: %w", missingTable(err))
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidateSlot, error) {
		var c candidateSlot
		err := row.Scan(&c.name, &c.active, &c.live)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("list engine slots: %w", missingTable(err))
	}
	return candidates, nil
}

// likePrefix is the LIKE pattern matching names that start with prefix,
// with LIKE's own wildcards in prefix escaped so an underscore matches an
// underscore.
func likePrefix(prefix string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return escaped + "%"
}
