package refusal_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/refusal"
)

// refusalClassesDoc decides the class vocabulary; the constants here are
// the core's copy of it, and these tests keep the two the same.
const refusalClassesDoc = "../../docs/refusal-classes.md"

// vocabularySection heads the table that decides every class and the
// consumer action each one asks for.
const vocabularySection = "## The decided vocabulary"

// Every class the core can hand out is one the document decided: a class
// added here without a row in the vocabulary table fails, and so does a
// class whose token drifted from the table's.
func TestEveryClassIsInTheDecidedVocabulary(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	start := strings.Index(string(raw), vocabularySection)
	require.NotEqual(t, -1, start, "docs/refusal-classes.md has the section %q", vocabularySection)
	table := string(raw)[start+len(vocabularySection):]
	if end := strings.Index(table, "\n## "); end != -1 {
		table = table[:end]
	}
	for _, class := range refusal.Classes() {
		assert.Contains(t, table, fmt.Sprintf("| `%s` |", class),
			"docs/refusal-classes.md has no vocabulary row for class %q", class)
	}
}

// causeTableHeading marks a section of docs/refusal-classes.md whose table
// is keyed on a typed cause, as against the table keyed on the refusal
// site; only the typed causes are the engine's, so only their class cells
// have to be classes the engine can name.
const causeTableHeading = "keyed on `"

// Every class cell of every cause table in the document is a class the
// engine can name: a cause table that classifies a cause outside Classes()
// — a class the verdict contract knows but no engine cause takes — fails
// here, so the closed set stays the set the document uses, whichever
// package's cause the row belongs to.
func TestEveryCauseTableClassIsACoreClass(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	tables := 0
	for _, section := range strings.Split(string(raw), "\n### ")[1:] {
		heading, body, _ := strings.Cut(section, "\n")
		if !strings.Contains(heading, causeTableHeading) {
			continue
		}
		tables++
		for cause, class := range classCells(t, body) {
			assert.True(t, class.Valid(),
				"docs/refusal-classes.md classifies %q under %q, which refusal.Classes() does not list", cause, class)
		}
	}
	assert.Positive(t, tables, "docs/refusal-classes.md has cause tables")
}

// classCells returns the first cell and the class cell of every row of every
// table in body whose header row names a `class` column; a table without
// one, such as a section's table of outcomes, contributes nothing.
func classCells(t *testing.T, body string) map[string]refusal.Class {
	t.Helper()
	cells := make(map[string]refusal.Class)
	inTable, classColumn := false, -1
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, "|") {
			inTable = false
			continue
		}
		columns := strings.Split(line, "|")
		if !inTable {
			inTable = true
			classColumn = slices.IndexFunc(columns, func(c string) bool { return strings.TrimSpace(c) == "`class`" })
			continue
		}
		if classColumn == -1 || strings.HasPrefix(strings.TrimSpace(columns[1]), "---") {
			continue
		}
		require.Greater(t, len(columns), classColumn, "row has a class cell: %q", line)
		cause := strings.Trim(strings.TrimSpace(columns[1]), "`")
		cells[cause] = refusal.Class(strings.Trim(strings.TrimSpace(columns[classColumn]), "`"))
	}
	require.NotEmpty(t, cells, "the section has a table with a `class` column")
	return cells
}

// The set is closed and distinct: no class is the zero value and no two
// share a token, since an importer switches on the token.
func TestClassesAreDistinctAndNonEmpty(t *testing.T) {
	seen := make(map[refusal.Class]struct{})
	for _, class := range refusal.Classes() {
		assert.NotEmpty(t, class)
		_, dup := seen[class]
		assert.False(t, dup, "class %q listed twice", class)
		seen[class] = struct{}{}
	}
}

// Valid admits exactly the listed classes: the empty class, which marks an
// error that is not a typed refusal, and a token outside the set are both
// invalid, so a class read back from stored output cannot route on a value
// the engine never hands out.
func TestValidAdmitsOnlyTheListedClasses(t *testing.T) {
	for _, class := range refusal.Classes() {
		assert.True(t, class.Valid(), "class %q", class)
	}
	assert.False(t, refusal.Class("").Valid())
	assert.False(t, refusal.Class("no-online-safety-problem").Valid(), "a verdict class no engine cause takes")
	assert.False(t, refusal.Class("Environmental").Valid(), "tokens are case-sensitive")
}
