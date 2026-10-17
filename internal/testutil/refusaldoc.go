package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// RefusalClassesDoc is the repository path of the routing contract that
// classifies every typed refusal cause the engine can raise. Each package
// that declares a cause type keeps its own cause table in this document
// honest with the readers below.
const RefusalClassesDoc = "docs/refusal-classes.md"

// RefusalCauseTable returns the text of the section of RefusalClassesDoc
// under heading, up to the next heading of the same level: one package's
// cause table, read from the repository checkout the test runs in. Two
// tables in the document list some of the same cause names under their own
// meanings, so a guard looks rows up in its own section alone.
func RefusalCauseTable(t *testing.T, heading string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), RefusalClassesDoc))
	require.NoError(t, err)
	doc := string(raw)
	start := strings.Index(doc, heading)
	require.NotEqual(t, -1, start, "%s has the section %q", RefusalClassesDoc, heading)
	section := doc[start+len(heading):]
	if end := strings.Index(section, "\n### "); end != -1 {
		section = section[:end]
	}
	return section
}

// RefusalCauseRow is the row keyed on cause in a cause table: the cause,
// what the refusal says, its class, and why.
func RefusalCauseRow(cause string) string {
	return fmt.Sprintf("| `%s` |", cause)
}

// RefusalClassCell returns the `class` cell of the row keyed on cause in a
// cause table whose columns are the cause, what it says, its class, and
// why; the test fails when the table has no such row.
func RefusalClassCell(t *testing.T, table, cause string) string {
	t.Helper()
	key := RefusalCauseRow(cause)
	for line := range strings.SplitSeq(table, "\n") {
		if !strings.HasPrefix(line, key) {
			continue
		}
		cells := strings.Split(line, "|")
		require.GreaterOrEqual(t, len(cells), 5, "row for %q has a class column", cause)
		return strings.Trim(strings.TrimSpace(cells[3]), "`")
	}
	require.Failf(t, "missing row", "%s has no row for cause %q", RefusalClassesDoc, cause)
	return ""
}

// repositoryRoot is the checkout this file belongs to, so a reader works
// from any package's test directory.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "the caller's source path is known")
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
