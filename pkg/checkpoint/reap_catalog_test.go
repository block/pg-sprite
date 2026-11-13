package checkpoint

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The reaper and the slot lock run on whatever pool the caller passes, so
// their reads cannot rely on the session's search_path: every relation
// other than the engine's own table, and every function, cast, and
// operator they spell, is pg_catalog-qualified (CO-9). This pins the SQL
// text itself; the integration test proves the behaviour under a
// lookalike = and LIKE on name.
func TestSlotReadsQualifyEveryRelationFunctionCastAndOperator(t *testing.T) {
	for name, query := range map[string]string{
		"candidateSlotsSQL":   candidateSlotsSQL,
		"liveRowSlotsSQL":     liveRowSlotsSQL,
		"databaseIdentitySQL": databaseIdentitySQL,
		"slotLockSQL":         slotLockSQL,
	} {
		t.Run(name, func(t *testing.T) {
			assertCatalogQualified(t, query)
		})
	}
}

var (
	qualifiedRelation  = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+(\S+)`)
	qualifiedCall      = regexp.MustCompile(`([A-Za-z_][A-Za-z_.0-9]*)\(`)
	qualifiedCast      = regexp.MustCompile(`::([A-Za-z_][A-Za-z_.0-9]*)`)
	qualifiedOperator  = regexp.MustCompile(`OPERATOR\(pg_catalog\.[^)]+\)`)
	bareOperatorSymbol = regexp.MustCompile(`[=<>!~|]`)
	bareOperatorWord   = regexp.MustCompile(`(?i)\b(?:LIKE|ILIKE|SIMILAR|IN|BETWEEN|DISTINCT)\b`)
)

// assertCatalogQualified fails on any relation, function, or cast in query
// not qualified with pg_catalog — the engine's own table excepted — and on
// any comparison not spelled OPERATOR(pg_catalog.…), the keyword forms
// that resolve an operator through the path (LIKE, IN, BETWEEN, IS
// DISTINCT FROM) included.
func assertCatalogQualified(t *testing.T, query string) {
	t.Helper()
	for _, match := range qualifiedRelation.FindAllStringSubmatch(query, -1) {
		if match[1] == tableIdent {
			continue
		}
		assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
			"relation %q is not qualified with pg_catalog in:\n%s", match[1], query)
	}
	for _, match := range qualifiedCall.FindAllStringSubmatch(query, -1) {
		switch match[1] {
		case "OPERATOR", "ANY", "ALL":
			continue
		}
		assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
			"function %q is not qualified with pg_catalog in:\n%s", match[1], query)
	}
	for _, match := range qualifiedCast.FindAllStringSubmatch(query, -1) {
		assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
			"cast to %q is not qualified with pg_catalog in:\n%s", match[1], query)
	}
	bare := qualifiedOperator.ReplaceAllString(query, "")
	assert.Empty(t, bareOperatorSymbol.FindAllString(bare, -1),
		"an operator is not spelled OPERATOR(pg_catalog.…) in:\n%s", query)
	assert.Empty(t, bareOperatorWord.FindAllString(bare, -1),
		"a keyword comparison resolves its operator through the path in:\n%s", query)
}
