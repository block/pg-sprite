package checkpoint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ensure runs on whatever pool the caller passes, so its reads cannot
// rely on the session's search_path: every relation, function, cast, and
// operator they spell is pg_catalog-qualified (CO-9). This pins the SQL
// text itself; the integration test proves the behaviour under a
// shadowing path.
func TestEnsureReadsQualifyEveryRelationFunctionCastAndOperator(t *testing.T) {
	queries := append(selectLiterals(t, "ensure.go"), selectLiterals(t, "columns.go")...)
	require.Len(t, queries, 3, "ensure.go holds the lock key and owner reads; columns.go holds the column read")

	relation := regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+(\S+)`)
	call := regexp.MustCompile(`([A-Za-z_][A-Za-z_.0-9]*)\(`)
	cast := regexp.MustCompile(`::([A-Za-z_][A-Za-z_.0-9]*)`)
	for _, query := range queries {
		for _, match := range relation.FindAllStringSubmatch(query, -1) {
			assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
				"relation %q is not qualified with pg_catalog in:\n%s", match[1], query)
		}
		for _, match := range call.FindAllStringSubmatch(query, -1) {
			if match[1] == "OPERATOR" {
				continue
			}
			assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
				"function %q is not qualified with pg_catalog in:\n%s", match[1], query)
		}
		for _, match := range cast.FindAllStringSubmatch(query, -1) {
			assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
				"cast to %q is not qualified with pg_catalog in:\n%s", match[1], query)
		}
		bare := strings.NewReplacer(
			"OPERATOR(pg_catalog.=)", "",
			"OPERATOR(pg_catalog.>)", "",
			"OPERATOR(pg_catalog.||)", "",
		).Replace(query)
		assert.NotContains(t, bare, "=", "a comparison is not spelled OPERATOR(pg_catalog.=) in:\n%s", query)
		assert.NotContains(t, bare, ">", "a comparison is not spelled OPERATOR(pg_catalog.>) in:\n%s", query)
		assert.NotContains(t, bare, "||", "a concatenation is not spelled OPERATOR(pg_catalog.||) in:\n%s", query)
	}
}

// selectLiterals collects every string literal in file whose text, after
// trimming, begins a SELECT.
func selectLiterals(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var queries []string
	ast.Inspect(parsed, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(lit.Value)
		require.NoError(t, err)
		if strings.HasPrefix(strings.TrimSpace(text), "SELECT") {
			queries = append(queries, text)
		}
		return true
	})
	return queries
}
