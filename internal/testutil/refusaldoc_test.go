package testutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The create-path section is the document's last cause table and is followed
// by ## sections; its table must end there, or a row moved into a later
// section would still satisfy the executor guards.
func TestRefusalCauseTableStopsAtTheNextHeading(t *testing.T) {
	table := RefusalCauseTable(t, "### `unsupported-statement` on the create path, keyed on `CreateShapeCause`")
	assert.NotContains(t, table, "\n## ", "the create-path table ends where the next section begins")
	assert.Contains(t, table, RefusalCauseRow("multiple-operations"), "the section's own rows are still read")
}
