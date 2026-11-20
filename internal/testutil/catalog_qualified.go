package testutil

import (
	"regexp"
	"slices"
	"strings"

	"github.com/stretchr/testify/assert"
)

var (
	catalogRelation      = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+(?:LATERAL\s+)?(\(\s*(?:SELECT|VALUES|WITH)\b|\S+)`)
	catalogSubquery      = regexp.MustCompile(`(?i)^\(\s*(?:SELECT|VALUES|WITH)\b`)
	catalogCall          = regexp.MustCompile(`([A-Za-z_][A-Za-z_.0-9]*)\(`)
	catalogCast          = regexp.MustCompile(`::([A-Za-z_][A-Za-z_.0-9]*)`)
	catalogOperator      = regexp.MustCompile(`OPERATOR\(pg_catalog\.[^)]+\)`)
	catalogStringLiteral = regexp.MustCompile(`'(?:[^']|'')*'`)
	catalogCountStar     = regexp.MustCompile(`\(\*\)`)
	catalogCastMarker    = regexp.MustCompile(`::`)
	// Every character PostgreSQL lets an operator name use.
	catalogOperatorChar = regexp.MustCompile(`[-+*/<>=~!@#%^&|` + "`" + `?]`)
	catalogOperatorWord = regexp.MustCompile(`(?i)\b(?:LIKE|ILIKE|SIMILAR|IN|BETWEEN|DISTINCT)\b`)
)

// catalogT is the part of *testing.T AssertCatalogQualified uses, so its
// own test can record a report rather than fail.
type catalogT interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertCatalogQualified fails on any relation, function, or cast in query
// not qualified with pg_catalog, the relations in own excepted, and on any
// operator not spelled OPERATOR(pg_catalog.…): every operator character
// outside one, and the keyword forms that resolve an operator through the
// path (LIKE, IN, BETWEEN, IS DISTINCT FROM). It is the text half of CO-9
// for a read that runs on a caller's pool, whatever its search_path; a
// lookalike pool proves the behaviour.
func AssertCatalogQualified(t catalogT, query string, own ...string) {
	t.Helper()
	for _, match := range catalogRelation.FindAllStringSubmatch(query, -1) {
		if slices.Contains(own, match[1]) {
			continue
		}
		// A subquery in FROM or JOIN, LATERAL or not, names no relation
		// itself; the relations and functions inside it are matched on
		// their own. A parenthesised join is not a subquery: its first
		// relation is matched without the parenthesis before it.
		if catalogSubquery.MatchString(match[1]) {
			continue
		}
		relation := strings.TrimLeft(match[1], "(")
		assert.True(t, strings.HasPrefix(relation, "pg_catalog."),
			"relation %q is not qualified with pg_catalog in:\n%s", relation, query)
	}
	for _, match := range catalogCall.FindAllStringSubmatch(query, -1) {
		switch match[1] {
		case "OPERATOR", "ANY", "ALL", "ARRAY":
			continue
		}
		assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
			"function %q is not qualified with pg_catalog in:\n%s", match[1], query)
	}
	for _, match := range catalogCast.FindAllStringSubmatch(query, -1) {
		assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
			"cast to %q is not qualified with pg_catalog in:\n%s", match[1], query)
	}
	bare := catalogOperator.ReplaceAllString(query, "")
	bare = catalogStringLiteral.ReplaceAllString(bare, "''")
	bare = catalogCountStar.ReplaceAllString(bare, "()")
	bare = catalogCastMarker.ReplaceAllString(bare, " ")
	assert.Empty(t, catalogOperatorChar.FindAllString(bare, -1),
		"an operator is not spelled OPERATOR(pg_catalog.…) in:\n%s", query)
	assert.Empty(t, catalogOperatorWord.FindAllString(bare, -1),
		"a keyword comparison resolves its operator through the path in:\n%s", query)
}
