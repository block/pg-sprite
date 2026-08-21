package verdict

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cliOutputExamplesDoc is the human-facing page that documents every
// refusal-reason token; this test keeps it honest the same way
// pkg/plan/docs_test.go keeps docs/plan-report.md honest.
const cliOutputExamplesDoc = "../../docs/cli-output-examples.md"

// Every refusal reason automation can meet must be documented: a Reason
// constant added without a row in the doc's refusal-reason table fails here.
func TestDocListsEveryRefusalReason(t *testing.T) {
	raw, err := os.ReadFile(cliOutputExamplesDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, r := range Reasons() {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", string(r)),
			"docs/cli-output-examples.md is missing a refusal-reason row for %q", r)
	}
}
