package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// With no session and no unfinished row, the text says there is neither,
// whether or not the database has a checkpoint table.
func TestRenderStatusWithNothingToShow(t *testing.T) {
	for _, state := range []checkpointTableState{checkpointTablePresent, checkpointTableAbsent} {
		text := renderStatus(statusReport{Sessions: []session{}, CheckpointTable: state, StoppedRuns: []statusRun{}})
		assert.Equal(t, "no pg-sprite sessions or stopped runs\n", text, "checkpoint table %s", state)
	}
}

// A stopped live run prints its phase and last save, its statement, its
// slot with the WAL it retains, and the two ways past it; a running one
// prints its pid and no next step.
func TestRenderStatusStoppedAndRunningRuns(t *testing.T) {
	retained := int64(523 << 20)
	saved := time.Date(2026, 11, 28, 10, 15, 2, 0, time.UTC)
	text := renderStatus(statusReport{
		Sessions:        []session{},
		CheckpointTable: checkpointTablePresent,
		StoppedRuns: []statusRun{
			{
				Table:     "public.orders",
				Statement: "ALTER TABLE public.orders ALTER COLUMN qty TYPE bigint",
				Phase:     "copying",
				UpdatedAt: saved,
				Slot: &statusSlot{
					Name:          "pgsprite_b32f988b",
					Exists:        true,
					WALStatus:     "reserved",
					RetainedBytes: &retained,
				},
			},
			{
				Table:      "public.ledger",
				Phase:      "catching_up",
				UpdatedAt:  saved,
				Running:    true,
				RunningPID: 4242,
				Slot:       &statusSlot{Name: "pgsprite_0badf00d"},
			},
		},
	})
	assert.Equal(t, "no active pg-sprite sessions\n"+
		"copy-and-swap runs with a checkpoint row: 2\n"+
		"  public.orders: stopped in phase copying, last saved 2026-11-28T10:15:02Z\n"+
		"    statement: ALTER TABLE public.orders ALTER COLUMN qty TYPE bigint\n"+
		"    replication slot pgsprite_b32f988b: inactive, wal_status reserved, retaining 523.0 MiB of WAL\n"+
		"    next: re-run the same pg-sprite migrate --copy-and-swap command to resume, or abandon the change with: pg-sprite discard public.orders\n"+
		"  public.ledger: running (pid 4242) in phase catching_up, last saved 2026-11-28T10:15:02Z\n"+
		"    replication slot pgsprite_0badf00d: gone; a resume creates a new slot and reconciles the copy against the source\n", text)
}

// A role that may not read the checkpoint table gets the sessions and a
// line saying the runs are unknown, never "no stopped runs".
func TestRenderStatusUnreadableCheckpointTable(t *testing.T) {
	text := renderStatus(statusReport{Sessions: []session{}, CheckpointTable: checkpointTablePermissionDenied, StoppedRuns: []statusRun{}})
	assert.Equal(t, "no active pg-sprite sessions\n"+
		"copy-and-swap runs: unknown: this role may not read pgsprite.pgsprite_checkpoint; ask for SELECT on it, or run status as the role that runs schema changes\n", text)
}
