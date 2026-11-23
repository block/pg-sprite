package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// reapReport is the reap command's output: every engine-prefixed slot of
// the connected database in exactly one list. Every list is always an
// array, empty when nothing is in it. A dry run fills WouldDrop and leaves
// Dropped empty; a real run fills Dropped and leaves WouldDrop empty, so a
// reader that skips dry_run cannot mistake a preview for a drop.
type reapReport struct {
	DryRun bool `json:"dry_run"`
	// CheckpointTableExists is false when the database has no checkpoint
	// table: no schema change has run in it, so it has no slot of the
	// engine's to reap and every list is empty.
	CheckpointTableExists bool `json:"checkpoint_table_exists"`
	// Stopped is true when a drop failed: the lists hold only the slots
	// sorted before the failure, and the command exits 1. A slot whose drop
	// went through before its publication was refused or failed to drop is
	// gone, so it is the last entry of Dropped, and the error names the
	// publication left.
	Stopped bool `json:"stopped"`
	// Dropped are the orphan slots the run dropped, publications included
	// except as Stopped says.
	Dropped []string `json:"dropped"`
	// WouldDrop are the orphan slots a real run would drop now.
	WouldDrop []string `json:"would_drop"`
	// Live slots are kept: a checkpoint row in a non-terminal phase names each.
	Live []string `json:"live"`
	// Active slots are kept: a process holds each.
	Active []string `json:"active"`
	// Foreign slots are kept: each wears the pgsprite_ prefix without the
	// engine's name shape.
	Foreign []string `json:"foreign"`
}

// newReapReport turns the store's sorting into the report, its Dropped
// list read as dropped or as would-drop by dryRun.
func newReapReport(dryRun bool, reaped checkpoint.Reaped) reapReport {
	report := reapReport{
		DryRun:                dryRun,
		CheckpointTableExists: true,
		Dropped:               []string{},
		WouldDrop:             []string{},
		Live:                  orEmpty(reaped.Live),
		Active:                orEmpty(reaped.Active),
		Foreign:               orEmpty(reaped.Foreign),
	}
	if dryRun {
		report.WouldDrop = orEmpty(reaped.Dropped)
	} else {
		report.Dropped = orEmpty(reaped.Dropped)
	}
	return report
}

// orEmpty returns names, or an empty list for nil, so the JSON carries an
// array and never null.
func orEmpty(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

// run reaps, or with --dry-run previews, the engine's orphan replication
// slots in the connected database. A database without the checkpoint table
// is reported, not refused: no schema change has run there, so nothing is
// the engine's to reap. A drop that fails partway still prints what was
// dropped before it, then returns the error.
func (c *ReapCmd) run(ctx context.Context, out io.Writer) error {
	cfg := c.Config()
	pool, err := dbconn.NewPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	if err != nil {
		return err
	}

	var reaped checkpoint.Reaped
	if c.DryRun {
		reaped, err = store.PreviewOrphanSlots(ctx)
	} else {
		reaped, err = store.ReapOrphanSlots(ctx, cfg)
	}
	if errors.Is(err, checkpoint.ErrTableMissing) {
		report := newReapReport(c.DryRun, checkpoint.Reaped{})
		report.CheckpointTableExists = false
		return c.write(out, report)
	}
	if err != nil && reapedNothing(reaped) {
		return fmt.Errorf("reap orphan slots: %w", err)
	}
	report := newReapReport(c.DryRun, reaped)
	report.Stopped = err != nil
	if writeErr := c.write(out, report); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	if err != nil {
		return fmt.Errorf("reap orphan slots: %w", err)
	}
	return nil
}

// reapedNothing reports whether a pass filed no slot in any list: a pass
// that failed before it sorted anything has no report worth printing.
func reapedNothing(reaped checkpoint.Reaped) bool {
	filed := len(reaped.Dropped) + len(reaped.Live) + len(reaped.Active) + len(reaped.Foreign)
	return filed == 0
}

// write prints the report as JSON or as text.
func (c *ReapCmd) write(out io.Writer, report reapReport) error {
	if c.JSON {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode reap report: %w", err)
		}
		if _, err := fmt.Fprintln(out, string(b)); err != nil {
			return fmt.Errorf("write reap report: %w", err)
		}
		return nil
	}
	if _, err := io.WriteString(out, renderReap(report)); err != nil {
		return fmt.Errorf("write reap report: %w", err)
	}
	return nil
}

// renderReap is the human-readable reap report: one section per list, each
// with its count and one slot per line.
func renderReap(report reapReport) string {
	var b strings.Builder
	if !report.CheckpointTableExists {
		b.WriteString("no checkpoint table in this database: no schema change has run here, so no slot is pg-sprite's to reap; nothing was dropped\n")
		return b.String()
	}
	if report.DryRun {
		writeReapSection(&b, "would drop (orphans, with their publications)", report.WouldDrop)
	} else {
		writeReapSection(&b, "dropped (orphans, with their publications)", report.Dropped)
	}
	writeReapSection(&b, "live (kept: a checkpoint row in a non-terminal phase names it)", report.Live)
	writeReapSection(&b, "active (kept: a process holds it)", report.Active)
	writeReapSection(&b, "foreign (kept: wears the pgsprite_ prefix without the engine's name shape)", report.Foreign)
	if report.Stopped {
		b.WriteString("stopped at a failed drop: the error names what was left in place; slots after it were not reached\n")
	}
	if report.DryRun {
		b.WriteString("dry run: nothing was dropped\n")
	}
	return b.String()
}

// writeReapSection writes one list under its label.
func writeReapSection(b *strings.Builder, label string, names []string) {
	fmt.Fprintf(b, "%s: %d\n", label, len(names))
	for _, name := range names {
		fmt.Fprintf(b, "  %s\n", name)
	}
}
