package cli

import (
	"bytes"
	"encoding/json"
	"testing"

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

func TestCapabilitiesTextListsEveryOperation(t *testing.T) {
	rows, err := capabilities.Rows()
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, writeCapabilitiesText(&out, palette{}, rows))
	for _, row := range rows {
		assert.Contains(t, out.String(), clip(row.Operation, 29))
	}
}
