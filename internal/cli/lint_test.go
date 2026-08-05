package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/lint"
)

func TestLintCleanScriptPrintsNothing(t *testing.T) {
	var out strings.Builder
	cmd := LintCmd{}
	err := cmd.runLint(strings.NewReader("CREATE TABLE t (id int)"), &out)
	require.NoError(t, err)
	assert.Empty(t, out.String())
}

// Warning findings are reported but do not fail the command; only
// error-severity findings flip the exit code.
func TestLintWarningsPassErrorsFail(t *testing.T) {
	var out strings.Builder
	cmd := LintCmd{JSON: true}
	err := cmd.runLint(strings.NewReader("CREATE INDEX t_c_idx ON t (c)"), &out)
	require.NoError(t, err)

	out.Reset()
	err = cmd.runLint(strings.NewReader(
		"ALTER TABLE t ADD CONSTRAINT no_overlap EXCLUDE USING gist (room WITH =)"), &out)
	require.ErrorIs(t, err, ErrLintFindings)

	var report lint.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Equal(t, lint.FormatVersion, report.FormatVersion)
	assert.Equal(t, 1, report.Errors)
	require.Len(t, report.Findings, 1)
	assert.Equal(t, lint.CodeUnsupportedOperation, report.Findings[0].Code)
}

func TestLintReadsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "change.sql")
	require.NoError(t, os.WriteFile(path,
		[]byte("ALTER TABLE t DROP COLUMN legacy"), 0o600))

	var out strings.Builder
	cmd := LintCmd{Path: path, JSON: true}
	require.NoError(t, cmd.runLint(strings.NewReader(""), &out))

	var report lint.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	require.Len(t, report.Findings, 1)
	assert.Equal(t, lint.CodeDestructive, report.Findings[0].Code)
	assert.Equal(t, 1, report.Warnings)
}

func TestLintParseFailureIsErrorNotFinding(t *testing.T) {
	var out strings.Builder
	cmd := LintCmd{}
	err := cmd.runLint(strings.NewReader("CREATE TABEL t (id int)"), &out)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrLintFindings)
}
