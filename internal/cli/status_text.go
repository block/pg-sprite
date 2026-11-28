package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/block/pg-sprite/pkg/checkpoint"
)

// renderStatus is the human-readable status report: the live sessions,
// then every run with a checkpoint row in a non-terminal phase, each
// stopped one ending with the two ways past it.
func renderStatus(r statusReport) string {
	var b strings.Builder
	unread := checkpointTableNote(r.CheckpointTable)
	if len(r.Sessions) == 0 && len(r.StoppedRuns) == 0 && unread == "" {
		b.WriteString("no pg-sprite sessions or stopped runs\n")
		return b.String()
	}
	writeSessions(&b, r.Sessions)
	if unread != "" {
		fmt.Fprintf(&b, "copy-and-swap runs: unknown: %s\n", unread)
		return b.String()
	}
	if len(r.StoppedRuns) == 0 {
		b.WriteString("no copy-and-swap runs with a checkpoint row\n")
		return b.String()
	}
	fmt.Fprintf(&b, "copy-and-swap runs with a checkpoint row: %d\n", len(r.StoppedRuns))
	for _, run := range r.StoppedRuns {
		writeStatusRun(&b, run)
	}
	return b.String()
}

// checkpointTableNote says why the runs could not be read, "" when they
// were or when there is no table to read them from.
func checkpointTableNote(state checkpointTableState) string {
	switch state {
	case checkpointTablePermissionDenied:
		return fmt.Sprintf("this role may not read %s.%s; ask for SELECT on it, or run status as the role that runs schema changes",
			checkpoint.SchemaName, checkpoint.TableName)
	case checkpointTableOlderEngine:
		return fmt.Sprintf("%s.%s was created by an older pg-sprite; run status with that version, or let a schema change of this version upgrade it",
			checkpoint.SchemaName, checkpoint.TableName)
	default:
		return ""
	}
}

// writeSessions writes the live session listing, or says there is none.
func writeSessions(b *strings.Builder, sessions []session) {
	if len(sessions) == 0 {
		b.WriteString("no active pg-sprite sessions\n")
		return
	}
	b.WriteString("active pg-sprite sessions:\n")
	for _, s := range sessions {
		fmt.Fprintf(b, "  pid=%d state=%s wait=%s running_for=%s query=%q\n",
			s.PID, s.State, s.WaitEvent, s.RunningFor, s.Query)
	}
}

// writeStatusRun writes one run: its state and phase, its statement, what
// it holds, and, for a stopped run, how to resume or abandon it.
func writeStatusRun(b *strings.Builder, run statusRun) {
	saved := run.UpdatedAt.Format(time.RFC3339)
	if run.Running {
		fmt.Fprintf(b, "  %s: running (pid %d) in phase %s, last saved %s\n", run.Table, run.RunningPID, run.Phase, saved)
	} else {
		fmt.Fprintf(b, "  %s: stopped in phase %s, last saved %s\n", run.Table, run.Phase, saved)
	}
	if run.Statement != "" {
		fmt.Fprintf(b, "    statement: %s\n", run.Statement)
	}
	if run.Slot != nil {
		fmt.Fprintf(b, "    replication slot %s: %s\n", run.Slot.Name, slotSummary(*run.Slot))
	}
	if run.WriteFence {
		fmt.Fprintf(b, "    write fence: installed; every write to %s is refused until the run finishes or is discarded\n", run.Table)
	}
	if !run.stopped() {
		return
	}
	resume := "re-run the same pg-sprite migrate --copy-and-swap command"
	if run.WriteFence {
		resume += " with --block-writes " + run.Table
	}
	fmt.Fprintf(b, "    next: %s to resume, or abandon the change with: pg-sprite discard %s\n", resume, run.Table)
	if run.Phase == checkpoint.PhaseCutover.String() {
		b.WriteString("    (discard is refused once the swap has committed; the same migrate command then finishes the change)\n")
	}
}

// slotSummary is one slot's line after its name.
func slotSummary(slot statusSlot) string {
	if !slot.Exists {
		return "gone; a resume creates a new slot and reconciles the copy against the source"
	}
	holder := "inactive"
	if slot.Active {
		holder = "active"
	}
	retained := "retained WAL unknown (the server cannot report it, as on a standby)"
	if slot.RetainedBytes != nil {
		retained = "retaining " + formatBytes(uint64(max(*slot.RetainedBytes, 0))) + " of WAL"
	}
	return fmt.Sprintf("%s, wal_status %s, %s", holder, slot.WALStatus, retained)
}
