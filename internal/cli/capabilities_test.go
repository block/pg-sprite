package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/capabilities"
)

func TestCapabilitiesJSON(t *testing.T) {
	cmd := CapabilitiesCmd{JSON: true, version: "test-version"}
	var out bytes.Buffer
	require.NoError(t, cmd.run(&out))

	var response capabilitiesResponse
	require.NoError(t, json.Unmarshal(out.Bytes(), &response))
	rows, err := capabilities.Rows()
	require.NoError(t, err)
	assert.NotEmpty(t, response.Version)
	assert.Equal(t, cmd.binaryVersion(), response.Version)
	assert.Len(t, response.Capabilities, len(rows))

	var tierTwo, diffRefused, toolOwned int
	for _, row := range response.Capabilities {
		if row.Tier == capabilities.TierTwo {
			tierTwo++
		}
		if row.FrontDoors.Diff == capabilities.DoorRefused {
			diffRefused++
		}
		if row.OwningToolClass != "" {
			toolOwned++
		}
	}
	assert.Positive(t, tierTwo)
	assert.Positive(t, diffRefused)
	assert.Positive(t, toolOwned)
}

// New is the only place the binary's version reaches the capabilities
// output, so the wiring is pinned through New rather than by constructing
// the command directly.
func TestNewWiresVersionIntoCapabilitiesJSON(t *testing.T) {
	c := New("v9.9.9-test")
	c.Capabilities.JSON = true
	var out bytes.Buffer
	require.NoError(t, c.Capabilities.run(&out))

	var response capabilitiesResponse
	require.NoError(t, json.Unmarshal(out.Bytes(), &response))
	assert.Equal(t, "v9.9.9-test", response.Version)
}

// The text table is display-only, but the rows are content: every operation
// is printed on its own line, no row is dropped, and the status mark sits in
// the same column on every line because everything before it is padded by
// single-width runes. The expectations are computed from the registry, not
// from the renderer's own helpers.
func TestCapabilitiesTextLayout(t *testing.T) {
	rows, err := capabilities.Rows()
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, writeCapabilitiesText(&out, palette{}, rows))
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, len(rows)+1, "one header line plus one line per row")

	header := lines[0]
	assert.True(t, strings.HasPrefix(header, "AREA "), header)
	assert.True(t, strings.HasSuffix(header, " STATUS"), header)
	markColumn := utf8.RuneCountInString(strings.TrimSuffix(header, "STATUS"))

	// "`DROP COLUMN`" is shorter than the operation column, so it must appear verbatim.
	assert.Contains(t, out.String(), " `DROP COLUMN` ")

	var sawEmptyOwner bool
	for i, row := range rows {
		line := lines[i+1]
		runes := []rune(line)
		require.Greater(t, len(runes), markColumn, line)
		assert.Equal(t, string(row.StatusMark), string(runes[markColumn:]), "status mark is the last column on %q", line)
		assert.Contains(t, line, " "+string(row.Tier)+" ", "tier is printed on %q", line)
		if row.OwningToolClass == "" {
			sawEmptyOwner = true
			assert.Contains(t, line, " — ", "an empty owner renders as an em dash on %q", line)
		} else {
			// Owners are clipped to the column, so pin a prefix short enough to survive.
			assert.Contains(t, line, string([]rune(row.OwningToolClass)[:5]), "owner is printed on %q", line)
		}
	}
	assert.True(t, sawEmptyOwner, "the registry has rows without an owning tool class")
}

func TestDoorLabel(t *testing.T) {
	tests := map[capabilities.FrontDoorStatus]string{
		capabilities.DoorSupported:     "yes",
		capabilities.DoorRefused:       "ref",
		capabilities.DoorNotApplicable: "n/a",
	}
	for status, want := range tests {
		assert.Equal(t, want, doorLabel(status), string(status))
	}
}

func TestClip(t *testing.T) {
	tests := []struct {
		name  string
		value string
		width int
		want  string
	}{
		{name: "shorter than width", value: "abc", width: 5, want: "abc"},
		{name: "exactly width", value: "abcde", width: 5, want: "abcde"},
		{name: "one over width", value: "abcdef", width: 5, want: "abcd…"},
		{name: "multi-byte runes count as one", value: "ééééé", width: 4, want: "ééé…"},
		{name: "leading spaces are content, not a reason to read past the value", value: "            ab", width: 13, want: "            …"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clip(tc.value, tc.width)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "\x00")
		})
	}
}
