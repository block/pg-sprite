package progress_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// progressReportDoc is the human-facing contract page this test keeps
// honest.
const progressReportDoc = "../../docs/progress-report.md"

// The doc's stated current version is the constant, not prose that can
// drift: a FormatVersion bump without the matching doc sentence fails here.
func TestDocStatesCurrentFormatVersion(t *testing.T) {
	raw, err := os.ReadFile(progressReportDoc)
	require.NoError(t, err)
	assert.Contains(t, string(raw), fmt.Sprintf("The current version is **%d**", progress.FormatVersion),
		"docs/progress-report.md's stated version drifted from progress.FormatVersion")
}
