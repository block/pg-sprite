package schemadiff

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

// The scratch build's two catalog reads compare oid with oid and oid with a
// bound parameter, so no planted overload can change their answers: there is
// no shadowing-path probe that could tell a qualified read from an unqualified
// one. The qualification is held by its text instead: every SELECT literal in
// desired_foreign_key.go names its relations under pg_catalog and spells its
// comparisons as OPERATOR(pg_catalog.…), so a relation or operator added to
// either read without its qualifier fails here.
func TestDesiredForeignKeyReadsQualifyEveryRelationAndOperator(t *testing.T) {
	queries := selectLiterals(t, "desired_foreign_key.go")
	require.Len(t, queries, 2, "desired_foreign_key.go holds the stand-in index read and the binding proof")

	relation := regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+(\S+)`)
	for _, query := range queries {
		for _, match := range relation.FindAllStringSubmatch(query, -1) {
			assert.True(t, strings.HasPrefix(match[1], "pg_catalog."),
				"relation %q is not qualified with pg_catalog in:\n%s", match[1], query)
		}
		bare := strings.NewReplacer("OPERATOR(pg_catalog.=)", "", "OPERATOR(pg_catalog.<>)", "").Replace(query)
		assert.NotContains(t, bare, "=", "a comparison is not spelled OPERATOR(pg_catalog.=) in:\n%s", query)
		assert.NotContains(t, bare, "<>", "a comparison is not spelled OPERATOR(pg_catalog.<>) in:\n%s", query)
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
