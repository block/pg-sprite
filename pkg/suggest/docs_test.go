package suggest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/suggest"
)

// suggestReportDoc is the human-facing contract page these tests keep
// honest.
const suggestReportDoc = "../../docs/suggest-report.md"

func readDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(suggestReportDoc)
	require.NoError(t, err)
	return string(raw)
}

// The doc's example reports are generated output, not prose: rebuilding
// them through the real advisory pipeline must reproduce the published
// JSON byte for byte (up to JSON equivalence). If this fails, regenerate
// the examples in docs/suggest-report.md.
func TestDocExamplesMatchPipelineOutput(t *testing.T) {
	doc := readDoc(t)
	blocks := regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(doc, -1)
	require.Len(t, blocks, 2, "the doc publishes one rewrite example and one guidance example")

	for i, sql := range []string{
		"CREATE INDEX t_c_idx ON t (c)",
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL, ADD COLUMN d int",
	} {
		report, err := suggest.Advise(sql)
		require.NoError(t, err)
		raw, err := json.Marshal(report)
		require.NoError(t, err)
		assert.JSONEq(t, blocks[i][1], string(raw),
			"docs/suggest-report.md example %d drifted from pipeline output", i+1)
	}
}

// Every vocabulary value the contract closes over must be documented: a
// constant added to the code without a row in docs/suggest-report.md (and
// a format_version decision) fails here.
func TestDocListsEveryVocabularyValue(t *testing.T) {
	doc := readDoc(t)
	var values []string
	for _, c := range suggest.Caveats() {
		values = append(values, string(c))
	}
	for _, g := range suggest.Guidances() {
		values = append(values, string(g))
	}
	for _, v := range values {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", v),
			"docs/suggest-report.md is missing a vocabulary row for %q", v)
	}
}
