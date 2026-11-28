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
	"github.com/block/pg-sprite/pkg/decode"
)

// reapFormatVersion identifies the reap report's JSON shape. It is bumped
// whenever a field is added, removed, or changes meaning; a consumer must
// reject a version it does not know.
const reapFormatVersion = 3

// reapReport is the reap command's output: every engine-prefixed slot of
// the connected database the pass reached, in exactly one list or in
// Failed, except that a slot dropped before its publication failed is in
// Dropped, Failed, and PublicationLeft. Every list is always an array,
// empty when nothing is in it. A dry run fills WouldDrop and leaves Dropped
// empty; a real run fills Dropped and leaves WouldDrop empty, so a reader
// that skips dry_run cannot mistake a preview for a drop.
type reapReport struct {
	FormatVersion int  `json:"format_version"`
	DryRun        bool `json:"dry_run"`
	// CheckpointTableExists is false when the database has no checkpoint
	// table: no row can show whether any slot is an orphan, so no slot was
	// examined and every list is empty.
	CheckpointTableExists bool `json:"checkpoint_table_exists"`
	// Stopped is true when the pass failed at a slot: the lists hold only
	// the slots sorted before it, Failed names it, and the command exits 1.
	Stopped bool `json:"stopped"`
	// Failed is the orphan the pass stopped at, empty when it did not stop.
	// Its slot may be gone: the drop removes the slot before its
	// publication. A slot whose drop went through before its publication
	// was refused or failed to drop is gone, so it is also the last entry of
	// Dropped and is named in PublicationLeft.
	Failed string `json:"failed"`
	// Dropped are the orphan slots the run dropped, publications included
	// except for any slot in PublicationLeft.
	Dropped []string `json:"dropped"`
	// PublicationLeft names the slot of Dropped whose same-named publication
	// the drop refused or failed to drop, left in place for the operator; it
	// holds at most the Failed slot, and is empty in a dry run.
	PublicationLeft []string `json:"publication_left"`
	// WouldDrop are the orphan slots a real run would drop now.
	WouldDrop []string `json:"would_drop"`
	// Live slots are kept: a checkpoint row in a non-terminal phase names each.
	Live []string `json:"live"`
	// LiveRuns names the table whose run owns each slot of Live, read
	// after the pass; a slot whose row went terminal since is not in it.
	LiveRuns []reapLiveRun `json:"live_runs"`
	// Active slots are kept: a process holds each.
	Active []string `json:"active"`
	// Foreign slots are kept: each wears the pgsprite_ prefix without the
	// engine's name shape or output plugin.
	Foreign []string `json:"foreign"`
}

// reapLiveRun is a kept live slot and the table whose run owns it, which
// `pg-sprite discard` takes to let the slot go.
type reapLiveRun struct {
	Slot  string `json:"slot"`
	Table string `json:"table"`
}

// newReapReport turns the store's sorting into the report.
func newReapReport(dryRun bool, reaped checkpoint.Reaped) reapReport {
	return reapReport{
		FormatVersion:         reapFormatVersion,
		DryRun:                dryRun,
		CheckpointTableExists: true,
		Failed:                reaped.Failed,
		Dropped:               orEmpty(reaped.Dropped),
		PublicationLeft:       []string{},
		WouldDrop:             orEmpty(reaped.WouldDrop),
		Live:                  orEmpty(reaped.Live),
		LiveRuns:              []reapLiveRun{},
		Active:                orEmpty(reaped.Active),
		Foreign:               orEmpty(reaped.Foreign),
	}
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
// is reported, not refused: no row there can show a slot is an orphan, so
// none is examined. A pass that stops at a slot still prints what it sorted
// before it and the slot it stopped at, then returns the error. A dry run
// that would drop opens one replication connection, as each drop does, so a
// role or URL a real run could not drop with fails the preview too.
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
	if err != nil && reapedNothing(reaped) {
		if errors.Is(err, checkpoint.ErrTableMissing) {
			report := newReapReport(c.DryRun, checkpoint.Reaped{})
			report.CheckpointTableExists = false
			return c.write(out, report)
		}
		return fmt.Errorf("reap orphan slots: %w", err)
	}
	if err == nil && len(reaped.WouldDrop) > 0 {
		err = probeReplication(ctx, cfg)
	}
	report := newReapReport(c.DryRun, reaped)
	if err == nil && len(reaped.Live) > 0 {
		report.LiveRuns, err = liveRuns(ctx, store, reaped.Live)
	}
	report.Stopped = reaped.Failed != ""
	if errors.Is(err, decode.ErrSlotDroppedPublicationLeft) {
		report.PublicationLeft = []string{reaped.Failed}
	}
	if writeErr := c.write(out, report); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	if err != nil {
		return fmt.Errorf("reap orphan slots: %w", err)
	}
	return nil
}

// reapedNothing reports whether a pass reached no slot at all: a pass that
// failed before it sorted anything has no report worth printing.
func reapedNothing(reaped checkpoint.Reaped) bool {
	filed := len(reaped.Dropped) + len(reaped.WouldDrop) + len(reaped.Live) + len(reaped.Active) + len(reaped.Foreign)
	return filed == 0 && reaped.Failed == ""
}

// liveRuns pairs each kept live slot with the table whose unfinished
// checkpoint row names it, in the order of live. The rows are read after
// the pass, so a slot whose row went terminal in between is left out.
func liveRuns(ctx context.Context, store *checkpoint.Store, live []string) ([]reapLiveRun, error) {
	rows, err := store.Unfinished(ctx)
	if err != nil {
		return []reapLiveRun{}, fmt.Errorf("read the runs that own the live slots: %w", err)
	}
	owners := make(map[string]string, len(rows))
	for _, row := range rows {
		if _, seen := owners[row.SlotName]; !seen {
			owners[row.SlotName] = row.Schema + "." + row.Table
		}
	}
	runs := []reapLiveRun{}
	for _, slot := range live {
		if table, ok := owners[slot]; ok {
			runs = append(runs, reapLiveRun{Slot: slot, Table: table})
		}
	}
	return runs, nil
}

// probeReplication opens and closes one replication connection from cfg,
// the connection every drop of a real run opens.
func probeReplication(ctx context.Context, cfg dbconn.Config) error {
	conn, err := dbconn.ConnectReplication(ctx, cfg)
	if err != nil {
		return fmt.Errorf("a real run could not drop these slots: %w", err)
	}
	if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("close replication probe: %w", err)
	}
	return nil
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
		b.WriteString("no checkpoint table in this database: no row can show whether a pgsprite_ slot is an orphan, so no slot was examined and nothing was dropped; a pgsprite_ slot in pg_replication_slots still keeps WAL\n")
		return b.String()
	}
	if report.DryRun {
		writeReapSection(&b, "would drop (orphans, with any publication of the engine's shape)", report.WouldDrop)
	} else {
		writeReapSection(&b, "dropped (orphans, with their publications except any left below)", report.Dropped)
		writeReapSection(&b, "publication left (its slot dropped above; the publication of its name was refused or failed to drop)", report.PublicationLeft)
	}
	writeLiveSection(&b, report.Live, report.LiveRuns)
	writeReapSection(&b, "active (kept: a process holds it)", report.Active)
	writeReapSection(&b, "foreign (kept: wears the pgsprite_ prefix without the engine's name shape or output plugin)", report.Foreign)
	if report.Stopped {
		fmt.Fprintf(&b, "stopped at %s: the pass failed there and the slot may be gone; slots after it were not reached\n", report.Failed)
	}
	if report.DryRun {
		b.WriteString("dry run: nothing was dropped\n")
	}
	return b.String()
}

// writeLiveSection writes the kept live slots, each beside the table whose
// run owns it and the discard that lets it go.
func writeLiveSection(b *strings.Builder, live []string, runs []reapLiveRun) {
	fmt.Fprintf(b, "live (kept: a checkpoint row in a non-terminal phase names it): %d\n", len(live))
	owners := make(map[string]string, len(runs))
	for _, run := range runs {
		owners[run.Slot] = run.Table
	}
	for _, name := range live {
		table, ok := owners[name]
		if !ok {
			fmt.Fprintf(b, "  %s\n", name)
			continue
		}
		fmt.Fprintf(b, "  %s  run on %s; resume it with the same migrate command, or let it go: pg-sprite discard %s\n", name, table, table)
	}
}

// writeReapSection writes one list under its label.
func writeReapSection(b *strings.Builder, label string, names []string) {
	fmt.Fprintf(b, "%s: %d\n", label, len(names))
	for _, name := range names {
		fmt.Fprintf(b, "  %s\n", name)
	}
}
