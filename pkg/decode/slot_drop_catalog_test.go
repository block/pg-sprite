package decode

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The drop's ownership check runs on whatever pool the caller passes, so
// its read cannot rely on the session's search_path: every relation,
// function, cast, and operator it spells is pg_catalog-qualified (CO-9).
// This pins the SQL text itself; the integration test proves the
// behaviour under a lookalike = on name.
func TestSlotIsOwnQualifiesEveryRelationFunctionCastAndOperator(t *testing.T) {
	relation := regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+(\S+)`)
	call := regexp.MustCompile(`([A-Za-z_][A-Za-z_.0-9]*)\(`)
	cast := regexp.MustCompile(`::([A-Za-z_][A-Za-z_.0-9]*)`)
	qualifiedOperator := regexp.MustCompile(`OPERATOR\(pg_catalog\.[^)]+\)`)
	bareSymbol := regexp.MustCompile(`[=<>!~|]`)
	bareWord := regexp.MustCompile(`(?i)\b(?:LIKE|ILIKE|SIMILAR|IN|BETWEEN|DISTINCT)\b`)

	for _, match := range relation.FindAllStringSubmatch(slotIsOwnSQL, -1) {
		assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
			"relation %q is not qualified with pg_catalog in:\n%s", match[1], slotIsOwnSQL)
	}
	for _, match := range call.FindAllStringSubmatch(slotIsOwnSQL, -1) {
		if match[1] == "OPERATOR" {
			continue
		}
		assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
			"function %q is not qualified with pg_catalog in:\n%s", match[1], slotIsOwnSQL)
	}
	for _, match := range cast.FindAllStringSubmatch(slotIsOwnSQL, -1) {
		assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
			"cast to %q is not qualified with pg_catalog in:\n%s", match[1], slotIsOwnSQL)
	}
	bare := qualifiedOperator.ReplaceAllString(slotIsOwnSQL, "")
	assert.Empty(t, bareSymbol.FindAllString(bare, -1),
		"an operator is not spelled OPERATOR(pg_catalog.…) in:\n%s", slotIsOwnSQL)
	assert.Empty(t, bareWord.FindAllString(bare, -1),
		"a keyword comparison resolves its operator through the path in:\n%s", slotIsOwnSQL)
}
