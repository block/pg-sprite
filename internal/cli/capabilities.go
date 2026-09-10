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

func writeCapabilitiesText(out io.Writer, pal palette, rows []capabilities.Row) error {
	if _, err := fmt.Fprintf(out, "%s\n", pal.bold(fmt.Sprintf("%-13s %-29s %-5s %-14s %-14s %-9s", "AREA", "OPERATION", "TIER", "BACKEND", "FRONT DOORS", "OWNER"))); err != nil {
		return fmt.Errorf("write capabilities: %w", err)
	}
	for _, row := range rows {
		frontDoors := fmt.Sprintf("m:%s d:%s", doorLabel(row.FrontDoors.Migrate), doorLabel(row.FrontDoors.Diff))
		owner := row.OwningToolClass
		if owner == "" {
			owner = "—"
		}
		tierMark := fmt.Sprintf("%s/%s", row.Tier, row.StatusMark)
		if _, err := fmt.Fprintf(out, "%-13s %-29s %-5s %-14s %-14s %-9s\n",
			clip(string(row.Area), 13), clip(row.Operation, 29), tierMark,
			clip(string(row.EnginePath), 14), frontDoors, clip(owner, 9)); err != nil {
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

func clip(value string, width int) string {
	if utf8.RuneCountInString(value) <= width {
		return value
	}
	return string([]rune(strings.TrimSpace(value))[:width-1]) + "…"
}
