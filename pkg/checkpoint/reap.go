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

// Reaped is what one pass of ReapOrphanSlots, or one PreviewOrphanSlots,
// found among the engine slots of the store's database, every slot it
// reached in exactly one list or in Failed, save the slot a drop took
// before leaving its publication, which is in Dropped and Failed both.
type Reaped struct {
	// Dropped slots had no checkpoint row, or a terminal one, and no holder:
	// orphans, dropped with their publications. Only ReapOrphanSlots fills it.
	// A pass that stops because a slot's publication was left after the
	// slot's drop lists that slot last, and its error names the publication.
	Dropped []string
	// WouldDrop slots are the orphans a pass would go on to drop now. Only
	// PreviewOrphanSlots fills it; nothing in it was dropped.
	WouldDrop []string
	// Failed is the orphan the pass stopped at when its lock, its row's
	// re-read, or its drop failed, and empty otherwise. The slot may be
	// gone: the drop removes the slot before its publication, and a drop the
	// server accepted can fail on the client's side. A slot the drop reports
	// gone with its publication left is in Dropped too.
	Failed string
	// Live slots are named by a checkpoint row in a non-terminal phase: a
	// schema change in flight or resumable owns each.
	Live []string
	// Active slots were held by a walsender or a backend when the reaper
	// went to drop them; whoever holds one is the judge of it, so the
	// reaper leaves it for a later pass.
	Active []string
	// Foreign slots wear the engine's prefix without its name shape or
	// without its output plugin; they are somebody else's and are reported,
	// never dropped.
	Foreign []string
}

// candidateSlotsSQL lists the slots of the store's database that wear the
// engine's prefix. Slots of other databases are never read: a slot is
// cluster state, but the row that owns it is per database, so only this
// database's rows can speak for this database's slots (D11). The active
// flag is advisory: pg_replication_slots reads shared memory, not a
// snapshot, so holders come and go underneath any statement, and the drop
// itself is what finds out whether a slot is held. The reaper runs on
// whatever pool the caller passes, so every comparison is spelled
// OPERATOR(pg_catalog.…) (LIKE is ~~): a lookalike = on name on the
// session's search_path would otherwise list another database's slots as
// this one's (CO-9). The output plugin is read so a slot of the engine's
// name shape that another tool made with another plugin is foreign.
const candidateSlotsSQL = `
SELECT slot_name, active, plugin
FROM pg_catalog.pg_replication_slots
WHERE database OPERATOR(pg_catalog.=) pg_catalog.current_database()
  AND slot_type OPERATOR(pg_catalog.=) 'logical'
  AND slot_name OPERATOR(pg_catalog.~~) $1
ORDER BY slot_name`

// liveRowSlotsSQL names, among the given slots, those a checkpoint row in a
// non-terminal phase owns. Terminal is judged by name, done or failed, and
// every other phase counts as live: a row a newer engine left in a phase
// this one does not know keeps its slot, since only that engine knows
// whether the run behind it still decodes from it. Its comparisons are
// pg_catalog-qualified for the same reason as candidateSlotsSQL's; NOT IN
// is its expansion, <> ALL.
const liveRowSlotsSQL = `
SELECT slot_name
FROM ` + tableIdent + `
WHERE slot_name OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.text[])
  AND phase OPERATOR(pg_catalog.<>) ALL (ARRAY[$2, $3]::pg_catalog.text[])`

// candidateSlot is one row of candidateSlotsSQL.
type candidateSlot struct {
	name   string
	active bool
	plugin string
}

// ReapOrphanSlots drops the engine's orphan replication slots in the store's
// database (ST-3). An orphan is a logical slot of the engine's name shape
// that no checkpoint row in a non-terminal phase names and that no process
// holds: a run makes its slot and commits the row that names it under the
// engine's lock on the slot's name, so a slot with no row was left by a run
// that ended without dropping it, and a slot named by a done or failed row
// by one whose cleanup did not finish.
//
// Ownership is judged twice. The slots are listed first and the rows that
// own them read in a later statement, which sorts the slots a live row
// plainly names out of the pass; then, under the engine's lock on the
// slot's name (WithSlotLock), the row is read again and the slot dropped,
// and that read is the one that decides: a run claims its slot and its row
// under the same lock, so a run that claimed the slot since the pass began
// keeps it, and a run claiming it now waits for the drop and creates
// afresh. Whether
// a slot is held is judged at the drop: each orphan goes through
// decode.DropIdleSlot on a replication connection built from cfg, which
// refuses a held slot instead of waiting for its holder and takes an idle
// slot's publication with it. Slots a live row names or a process holds,
// and slots that wear the prefix without the engine's name shape or output
// plugin, are reported and left as found. The drops stop at the first
// failure, reporting what was dropped before it and naming the slot it
// stopped at in Failed; a slot dropped whose publication the drop then
// refused or failed to drop (decode.ErrSlotDroppedPublicationLeft) is in
// Dropped too, since it is gone, and the error names the publication left.
// Two reapers over one database take turns at each slot; the one that
// waited finds the slot gone and reports the drop as its own too.
//
// A database where Ensure has never run has no rows to speak for its
// slots, so the pass is refused with ErrTableMissing rather than read every
// slot as an orphan. The reads that decide a slot's fate, the drop's
// included, do not consult the store pool's search_path, so a caller need
// not pin it.
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
		if !reaped.fileUnlessOrphan(c, live) {
			continue
		}
		if err := s.dropIfStillOrphan(ctx, cfg, c.name, &reaped); err != nil {
			reaped.Failed = c.name
			return reaped, err
		}
	}
	return reaped, nil
}

// PreviewOrphanSlots reports what ReapOrphanSlots would make of the engine
// slots of the store's database right now, dropping nothing: WouldDrop names
// the orphans a pass would go on to drop, and the other lists are the slots
// it would keep. It reads the same slot listing and live rows and sorts
// them with the same rule, but takes no slot lock and writes nothing, so it
// is a snapshot and not a promise: by the time a pass runs, a run may have
// claimed an orphan, which the pass then keeps as live, or a process may
// hold it, which the pass then keeps as active. A database where Ensure has
// never run is refused with ErrTableMissing, as ReapOrphanSlots refuses it.
func (s *Store) PreviewOrphanSlots(ctx context.Context) (Reaped, error) {
	candidates, err := s.candidateSlots(ctx)
	if err != nil {
		return Reaped{}, err
	}
	live, err := s.liveRowSlots(ctx, slotNames(candidates))
	if err != nil {
		return Reaped{}, err
	}
	var preview Reaped
	for _, c := range candidates {
		if preview.fileUnlessOrphan(c, live) {
			preview.WouldDrop = append(preview.WouldDrop, c.name)
		}
	}
	return preview, nil
}

// fileUnlessOrphan files a candidate the pass keeps under the list that
// keeps it and reports false, or reports true and files nothing for an
// orphan, which the caller drops or previews. live is the candidates'
// live-row read. ReapOrphanSlots and PreviewOrphanSlots both sort through
// it, so a preview never disagrees with a pass over the same reads.
func (r *Reaped) fileUnlessOrphan(c candidateSlot, live map[string]bool) bool {
	switch {
	case !decode.IsEngineSlotName(c.name):
		r.Foreign = append(r.Foreign, c.name)
	case c.plugin != decode.OutputPlugin:
		// Every engine slot decodes with pgoutput, so a slot of the engine's
		// name shape with another plugin was made by another tool.
		r.Foreign = append(r.Foreign, c.name)
	case live[c.name]:
		// INV: ST-3 — a row in flight owns its slot; the reaper never
		// drops a slot a schema change can still resume on.
		r.Live = append(r.Live, c.name)
	case c.active:
		r.Active = append(r.Active, c.name)
	default:
		return true
	}
	return false
}

// dropIfStillOrphan re-reads the slot's row and then drops the slot without
// waiting for a holder, both under the engine's lock on the slot's name,
// filing the slot under the list the outcome names.
func (s *Store) dropIfStillOrphan(ctx context.Context, cfg dbconn.Config, name string, reaped *Reaped) error {
	return s.WithSlotLock(ctx, cfg, name, func(ctx context.Context) error {
		live, err := s.liveRowSlots(ctx, []string{name})
		if err != nil {
			return err
		}
		if live[name] {
			// INV: ST-3 — the row is read under the slot's lock right before
			// the drop, so a run that claimed the slot since the pass began
			// keeps it, and one claiming it now waits for the drop.
			reaped.Live = append(reaped.Live, name)
			return nil
		}
		err = decode.DropIdleSlot(ctx, cfg, s.pool, name)
		if errors.Is(err, decode.ErrSlotActive) {
			reaped.Active = append(reaped.Active, name)
			return nil
		}
		if errors.Is(err, decode.ErrSlotDroppedPublicationLeft) {
			// The slot is gone and the report says so; the publication the
			// drop left is the operator's to look at, so the pass stops.
			reaped.Dropped = append(reaped.Dropped, name)
			return fmt.Errorf("reap orphan slot %s: %w", name, err)
		}
		if err != nil {
			return fmt.Errorf("reap orphan slot %s: %w", name, err)
		}
		reaped.Dropped = append(reaped.Dropped, name)
		return nil
	})
}

// candidateSlots reads the engine-prefixed slots of the store's database.
func (s *Store) candidateSlots(ctx context.Context) ([]candidateSlot, error) {
	rows, err := s.pool.Query(ctx, candidateSlotsSQL, likePrefix(decode.SlotNamePrefix))
	if err != nil {
		return nil, fmt.Errorf("list engine slots: %w", err)
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidateSlot, error) {
		var c candidateSlot
		err := row.Scan(&c.name, &c.active, &c.plugin)
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
		return nil, fmt.Errorf("read slot owners: %w", tableUnusable(err))
	}
	owned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read slot owners: %w", tableUnusable(err))
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
