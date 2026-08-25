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
