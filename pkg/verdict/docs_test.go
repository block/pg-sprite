package verdict

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cliOutputExamplesDoc is the human-facing page that documents every
// refusal-reason token; this test keeps it honest the same way
// pkg/plan/docs_test.go keeps docs/plan-report.md honest.
const cliOutputExamplesDoc = "../../docs/cli-output-examples.md"

// refusalClassesDoc is the routing contract that classifies every refusal
// reason; a reason without a class row there cannot be routed by consumers.
const refusalClassesDoc = "../../docs/refusal-classes.md"

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

// Every refusal reason must be classified: a Reason constant added without a
// place in the refusal-class map fails here, so a new reason cannot land
// without a routing decision. A reason is classified either by a row in the
// site-keyed table or by its own cause-keyed table, whose heading names it.
func TestRefusalClassesDocListsEveryRefusalReason(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, r := range Reasons() {
		row := fmt.Sprintf("| `%s` |", string(r))
		causeTable := fmt.Sprintf("### `%s`, keyed on", string(r))
		assert.True(t, strings.Contains(doc, row) || strings.Contains(doc, causeTable),
			"docs/refusal-classes.md has neither a class row nor a cause table for refusal reason %q", r)
	}
}
