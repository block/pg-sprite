package executor_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

// executionModelDoc is the human-facing contract page this test keeps honest.
const executionModelDoc = "../../docs/execution-model.md"

// Every step kind automation can branch on must be named in the execution
// model: a StepKind added to the code without the doc naming it fails here.
func TestDocNamesEveryStepKind(t *testing.T) {
	raw, err := os.ReadFile(executionModelDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, k := range executor.StepKinds() {
		assert.Contains(t, doc, fmt.Sprintf("`%s`", k),
			"docs/execution-model.md does not name step kind %q", k)
	}
}

// Every outcome code automation can branch on must be named in the
// execution model: a Code added to the vocabulary without the doc naming
// it fails here.
func TestDocNamesEveryOutcomeCode(t *testing.T) {
	raw, err := os.ReadFile(executionModelDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, c := range executor.Codes() {
		assert.Contains(t, doc, fmt.Sprintf("`%s`", c),
			"docs/execution-model.md does not name outcome code %q", c)
	}
}

// The closed set has no duplicates: a code pasted twice would silently
// shadow a missing entry.
func TestCodesAreUnique(t *testing.T) {
	seen := make(map[executor.Code]struct{})
	for _, c := range executor.Codes() {
		_, dup := seen[c]
		assert.False(t, dup, "duplicate outcome code %q", c)
		seen[c] = struct{}{}
	}
}
