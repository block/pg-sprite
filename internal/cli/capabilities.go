package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/block/pg-sprite/pkg/capabilities"
)

type capabilitiesResponse struct {
	Version      string             `json:"version"`
	Capabilities []capabilities.Row `json:"capabilities"`
}

func (c *CapabilitiesCmd) run(out io.Writer) error {
	rows, err := capabilities.Rows()
	if err != nil {
		return fmt.Errorf("load capabilities: %w", err)
	}
	if c.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(capabilitiesResponse{Version: c.binaryVersion(), Capabilities: rows}); err != nil {
			return fmt.Errorf("write capabilities JSON: %w", err)
		}
		return nil
	}
	return writeCapabilitiesText(out, c.palette(out), rows)
}

// capabilitiesRowFormat lays out one text row. Every padded column holds
// only single-width runes, so rune-counted padding lines up with the
// terminal's cells; the status mark is a double-width glyph, so it is the
// unpadded last column rather than a padded one that would shift everything
// after it by a cell.
const capabilitiesRowFormat = "%-13s %-29s %-4s %-14s %-14s %-9s %s\n"

func writeCapabilitiesText(out io.Writer, pal palette, rows []capabilities.Row) error {
	header := fmt.Sprintf(capabilitiesRowFormat, "AREA", "OPERATION", "TIER", "BACKEND", "FRONT DOORS", "OWNER", "STATUS")
	if _, err := fmt.Fprint(out, pal.bold(strings.TrimSuffix(header, "\n"))+"\n"); err != nil {
		return fmt.Errorf("write capabilities: %w", err)
	}
	for _, row := range rows {
		frontDoors := fmt.Sprintf("m:%s d:%s", doorLabel(row.FrontDoors.Migrate), doorLabel(row.FrontDoors.Diff))
		owner := row.OwningToolClass
		if owner == "" {
			owner = "—"
		}
		if _, err := fmt.Fprintf(out, capabilitiesRowFormat,
			clip(string(row.Area), 13), clip(row.Operation, 29), row.Tier,
			clip(string(row.EnginePath), 14), frontDoors, clip(owner, 9), row.StatusMark); err != nil {
			return fmt.Errorf("write capabilities: %w", err)
		}
	}
	return nil
}

func doorLabel(status capabilities.FrontDoorStatus) string {
	switch status {
	case capabilities.DoorSupported:
		return "yes"
	case capabilities.DoorRefused:
		return "ref"
	case capabilities.DoorNotApplicable:
		return "n/a"
	}
	return string(status)
}

// clip truncates value to width runes, spending the last rune on an
// ellipsis. The guard and the slice measure the same string, so the slice
// bound is always within range.
func clip(value string, width int) string {
	if utf8.RuneCountInString(value) <= width {
		return value
	}
	return string([]rune(value)[:width-1]) + "…"
}
