package suggest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
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
// constant added to the code without its entry in docs/suggest-report.md
// (and a format_version decision) fails here. Caveats are table rows;
// guidance codes are headings, because each one is a stable anchor the
// CLI's docs: lines link to.
func TestDocListsEveryVocabularyValue(t *testing.T) {
	doc := readDoc(t)
	for _, c := range suggest.Caveats() {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", c),
			"docs/suggest-report.md is missing a vocabulary row for %q", c)
	}
	for _, g := range suggest.Guidances() {
		assert.Contains(t, doc, fmt.Sprintf("### `%s`", g),
			"docs/suggest-report.md is missing an anchored heading for %q", g)
	}
	// The CLI's docs: line links the caveats section itself
	// (#caveats-caveats); pin the heading that produces the anchor so a
	// rename cannot silently break the printed link.
	assert.Contains(t, doc, "## Caveats (`caveats`)",
		"the caveats heading is a stable anchor the CLI's text output links")
}

// The add-integer-primary-key steps link a section of the primary key
// guide by its anchor, and a renamed heading would break the link with
// nothing else failing, so the anchor the link names must be a heading of
// the guide.
func TestAddIntegerPrimaryKeyLinksAHeadingOfTheGuide(t *testing.T) {
	link := regexp.MustCompile(`docs/primary-keys\.md#([a-z0-9-]+)`).FindStringSubmatch(suggest.ManualPath(suggest.GuidanceAddIntegerPrimaryKey))
	require.Len(t, link, 2, "the steps link the primary key guide")
	assert.Contains(t, headingAnchors(t, "../../docs/primary-keys.md"), link[1],
		"the linked anchor is a heading of docs/primary-keys.md")
}

// The attach guidance's last hop from the CLI's docs: line, and the pages
// that restate it, link the online-DDL reference's safe pattern by its
// anchor, so every such anchor must be a heading of the reference.
func TestAttachPartitionLinksAHeadingOfTheReference(t *testing.T) {
	const reference = "../../docs/postgres-online-ddl-reference.md"
	anchors := headingAnchors(t, reference)
	sources := map[string]*regexp.Regexp{
		suggestReportDoc:            regexp.MustCompile(`postgres-online-ddl-reference\.md#([a-z0-9-]+)`),
		"../../docs/limitations.md": regexp.MustCompile(`postgres-online-ddl-reference\.md#([a-z0-9-]+)`),
		reference:                   regexp.MustCompile(`\]\(#(safe-pattern-attach[a-z0-9-]*)\)`),
	}
	for path, link := range sources {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		found := link.FindAllStringSubmatch(string(raw), -1)
		require.NotEmpty(t, found, "%s links the reference", path)
		for _, match := range found {
			assert.Contains(t, anchors, match[1], "%s links #%s, which is not a heading of the reference", path, match[1])
		}
	}
}

// headingAnchors derives the anchor GitHub gives each heading of the
// Markdown page at path.
func headingAnchors(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var anchors []string
	for _, heading := range regexp.MustCompile(`(?m)^#+ (.+)$`).FindAllStringSubmatch(string(raw), -1) {
		anchor := strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9 -]`).ReplaceAllString(heading[1], ""))
		anchors = append(anchors, strings.ReplaceAll(anchor, " ", "-"))
	}
	return anchors
}

// The doc's stated current version is the constant, not prose that can
// drift: a FormatVersion bump without the matching doc sentence fails here.
func TestDocStatesCurrentFormatVersion(t *testing.T) {
	doc := readDoc(t)
	assert.Contains(t, doc, fmt.Sprintf("The current version is **%d**", suggest.FormatVersion),
		"docs/suggest-report.md's stated version drifted from suggest.FormatVersion")
}
