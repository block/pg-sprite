package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The status report's documentation states the format version the command
// prints, in its prose and in its field table, so a consumer that checks
// the version against the docs accepts exactly the shape it gets.
func TestDocStatesCurrentStatusFormatVersion(t *testing.T) {
	raw, err := os.ReadFile("../../docs/cli.md")
	require.NoError(t, err)
	doc := string(raw)
	assert.Contains(t, doc, fmt.Sprintf("`status --json` prints one object, `format_version` %d.", statusFormatVersion))
	assert.Contains(t, doc, fmt.Sprintf("| `format_version` | The report's shape, %d |", statusFormatVersion))
}
