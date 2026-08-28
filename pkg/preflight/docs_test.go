package preflight

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// proofTypeDocs are the hand-maintained registries of proof types; each one
// must list every proof type this package exports. Three prose lists drift
// independently, so this test pins them the same way pkg/verdict and
// pkg/plan pin their contract pages.
var proofTypeDocs = []string{
	"../../SAFETY.md",
	"../../.agents/checks/review.md",
	"../../docs/tcb-model.md",
}

// Every proof type exported by this package must appear in every registry
// document: a new proof type added without updating all three lists fails
// here. Extend the slice when a new proof type lands.
func TestDocsListEveryProofType(t *testing.T) {
	proofTypes := []string{"PreflightedTable", "AbsentTarget", "CreationRole"}
	for _, doc := range proofTypeDocs {
		raw, err := os.ReadFile(doc)
		require.NoError(t, err)
		for _, name := range proofTypes {
			assert.Contains(t, string(raw), name,
				"%s is missing the proof type %s", doc, name)
		}
	}
}
