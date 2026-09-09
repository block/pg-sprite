package schemadiff

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capabilitiesDoc is the capability statement whose operator-owned recipe
// shows the enumeration owners are told they may copy.
const capabilitiesDoc = "../../docs/capabilities.md"

// The recipe's SQL block is the query, spelled for an 'app' schema and
// annotated: a change to listManagedTablesSQL that the doc does not follow
// fails here, so the copy an owner pastes cannot drift from the copy pull
// runs.
func TestCapabilitiesDocShowsTheManagedTablesQuery(t *testing.T) {
	raw, err := os.ReadFile(capabilitiesDoc)
	require.NoError(t, err)
	doc := string(raw)
	_, section, found := strings.Cut(doc, "## Deliberately operator-owned")
	require.True(t, found, "docs/capabilities.md has no Deliberately operator-owned section")
	_, afterFence, found := strings.Cut(section, "```sql\n")
	require.True(t, found, "the operator-owned section has no sql block")
	block, _, found := strings.Cut(afterFence, "```")
	require.True(t, found, "the sql block is not closed")

	documented := strings.ReplaceAll(block, "'app'", "$1")
	assert.Equal(t, normalizeSQL(listManagedTablesSQL), normalizeSQL(documented),
		"the operator-owned recipe's SQL drifted from listManagedTablesSQL")
}

var (
	sqlLineComment = regexp.MustCompile(`--[^\n]*`)
	sqlWhitespace  = regexp.MustCompile(`\s+`)
)

// normalizeSQL strips comments, layout, and a trailing semicolon so two
// spellings of one statement compare by token sequence.
func normalizeSQL(sql string) string {
	s := sqlLineComment.ReplaceAllString(sql, "")
	s = sqlWhitespace.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "( ", "(")
	s = strings.ReplaceAll(s, " )", ")")
	s = strings.TrimSpace(s)
	return strings.TrimSuffix(s, ";")
}
