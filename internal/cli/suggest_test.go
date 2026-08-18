package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/suggest"
)

func TestSuggestCleanScriptPrintsNothing(t *testing.T) {
	var out strings.Builder
	cmd := SuggestCmd{}
	err := cmd.runSuggest(strings.NewReader("CREATE TABLE t (id int)"), &out)
	require.NoError(t, err)
	assert.Empty(t, out.String())
}

// Suggest is advisory: a script full of findings still exits zero — lint
// owns the gate.
func TestSuggestAlwaysExitsZeroOnValidScripts(t *testing.T) {
	var out strings.Builder
	cmd := SuggestCmd{JSON: true}
	err := cmd.runSuggest(strings.NewReader(`
		CREATE INDEX t_c_idx ON t (c);
		ALTER TABLE t ADD CONSTRAINT no_overlap EXCLUDE USING gist (room WITH =);
	`), &out)
	require.NoError(t, err)

	var report suggest.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Equal(t, suggest.FormatVersion, report.FormatVersion)
	require.Len(t, report.Suggestions, 1, "the refused statement yields no advice")
	assert.Equal(t, 1, report.Suggestions[0].Statement)
}

func TestSuggestReadsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "change.sql")
	require.NoError(t, os.WriteFile(path,
		[]byte("ALTER TABLE t ALTER COLUMN c SET NOT NULL"), 0o600))

	var out strings.Builder
	cmd := SuggestCmd{Path: path, JSON: true}
	require.NoError(t, cmd.runSuggest(strings.NewReader(""), &out))

	var report suggest.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	require.Len(t, report.Suggestions, 1)
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatSeparateTransactions, suggest.CaveatValidationScan,
			suggest.CaveatScaffoldConstraintOnFailure},
		report.Suggestions[0].Caveats)
}

func TestSuggestParseFailureIsError(t *testing.T) {
	var out strings.Builder
	cmd := SuggestCmd{}
	err := cmd.runSuggest(strings.NewReader("CREATE TABEL t (id int)"), &out)
	require.Error(t, err)
}

// The text rendering is this renderer's own unit test: the risky statement
// leads under the conventional name:line:column: label, and each
// suggestion shows the safer sequence and its caveats.
func TestSuggestTextRendering(t *testing.T) {
	var out strings.Builder
	cmd := SuggestCmd{}
	err := cmd.runSuggest(strings.NewReader("CREATE INDEX t_c_idx ON t (c)"), &out)
	require.NoError(t, err)
	text := out.String()
	assert.Contains(t, text, "<stdin>:1:1:")
	assert.Contains(t, text, "CONCURRENTLY")
	assert.Contains(t, text, string(suggest.CaveatNonTransactional))
	assert.Contains(t, text, string(suggest.CaveatInvalidIndexOnFailure))
}

// A suggestion without a constructible rewrite renders its guidance code —
// the manual path — instead of an empty safer-form block.
func TestSuggestTextRendersGuidance(t *testing.T) {
	var out strings.Builder
	cmd := SuggestCmd{}
	err := cmd.runSuggest(strings.NewReader(
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL, ADD COLUMN d int"), &out)
	require.NoError(t, err)
	text := out.String()
	assert.Contains(t, text, string(suggest.GuidanceSplitStatement))
	assert.NotContains(t, text, "safer form")
}
