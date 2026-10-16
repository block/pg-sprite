package refusal_test

import (
	"fmt"
	"os"
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
