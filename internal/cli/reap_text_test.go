package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A dry run's text names the slots under "would drop" and ends by saying
// nothing was dropped, so an operator reading it cannot take the preview
// for a drop.
func TestRenderReapDryRunSaysNothingWasDropped(t *testing.T) {
	text := renderReap(reapReport{
		DryRun:                true,
		CheckpointTableExists: true,
		WouldDrop:             []string{"pgsprite_deadbeef"},
		Live:                  []string{},
		Active:                []string{},
		Foreign:               []string{},
	})
	assert.Equal(t, "would drop (orphans, with any publication of the engine's shape): 1\n"+
		"  pgsprite_deadbeef\n"+
		"live (kept: a checkpoint row in a non-terminal phase names it): 0\n"+
		"active (kept: a process holds it): 0\n"+
		"foreign (kept: wears the pgsprite_ prefix without the engine's name shape or output plugin): 0\n"+
		"dry run: nothing was dropped\n", text)
}

// A kept live slot is printed beside the table whose run owns it and the
// discard that lets it go; a live slot whose owner was not read is
// printed alone.
func TestRenderReapNamesTheRunThatOwnsALiveSlot(t *testing.T) {
	text := renderReap(reapReport{
		DryRun:                true,
		CheckpointTableExists: true,
		WouldDrop:             []string{},
		Live:                  []string{"pgsprite_cafebabe", "pgsprite_0badf00d"},
		LiveRuns:              []reapLiveRun{{Slot: "pgsprite_cafebabe", Table: "public.orders"}},
		Active:                []string{},
		Foreign:               []string{},
	})
	assert.Equal(t, "would drop (orphans, with any publication of the engine's shape): 0\n"+
		"live (kept: a checkpoint row in a non-terminal phase names it): 2\n"+
		"  pgsprite_cafebabe  run on public.orders; resume it with the same migrate command, or let it go: pg-sprite discard public.orders\n"+
		"  pgsprite_0badf00d\n"+
		"active (kept: a process holds it): 0\n"+
		"foreign (kept: wears the pgsprite_ prefix without the engine's name shape or output plugin): 0\n"+
		"dry run: nothing was dropped\n", text)
}

// A stopped pass names the slot it stopped at and says the slots after it
// were not reached; a real run has no dry-run footer.
func TestRenderReapStoppedPassNamesTheSlotItStoppedAt(t *testing.T) {
	text := renderReap(reapReport{
		CheckpointTableExists: true,
		Stopped:               true,
		Failed:                "pgsprite_ffff0000",
		Dropped:               []string{"pgsprite_0000aaaa"},
		PublicationLeft:       []string{},
		Live:                  []string{},
		Active:                []string{},
		Foreign:               []string{},
	})
	assert.Equal(t, "dropped (orphans, with their publications except any left below): 1\n"+
		"  pgsprite_0000aaaa\n"+
		"publication left (its slot dropped above; the publication of its name was refused or failed to drop): 0\n"+
		"live (kept: a checkpoint row in a non-terminal phase names it): 0\n"+
		"active (kept: a process holds it): 0\n"+
		"foreign (kept: wears the pgsprite_ prefix without the engine's name shape or output plugin): 0\n"+
		"stopped at pgsprite_ffff0000: the pass failed there and the slot may be gone; slots after it were not reached\n", text)
}

// Without the checkpoint table the text says no slot was examined, rather
// than that no slot is the engine's: an engine slot in a database whose
// table was dropped still keeps WAL.
func TestRenderReapWithoutTheCheckpointTableSaysNoSlotWasExamined(t *testing.T) {
	text := renderReap(reapReport{})
	assert.Equal(t, "no checkpoint table in this database: no row can show whether a pgsprite_ slot is an orphan, "+
		"so no slot was examined and nothing was dropped; a pgsprite_ slot in pg_replication_slots still keeps WAL\n", text)
}
