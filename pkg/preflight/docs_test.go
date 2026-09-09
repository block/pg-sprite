package preflight_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/preflight"
)

// refusalClassesDoc is the routing contract that classifies every
// partitioned-parent refusal cause; this test keeps its cause table honest.
const refusalClassesDoc = "../../docs/refusal-classes.md"

// Every partition refusal cause automation can branch on must have a row in
// the refusal-class map: a cause added to the code without a classification
// fails here.
func TestRefusalClassesDocListsEveryPartitionCause(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, cause := range preflight.PartitionRefusalCauses() {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", cause),
			"docs/refusal-classes.md has no class row for partition refusal cause %q", cause)
	}
}

// The closed set is complete: every PartitionRefusalCause constant declared
// in partition.go is enumerated by PartitionRefusalCauses(). A constant
// added without its entry would pass the doc test above (which walks the
// accessor) while never being classified.
func TestPartitionRefusalCausesEnumerateEveryDeclaredCause(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "partition.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	declared := make(map[preflight.PartitionRefusalCause]struct{})
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || !isPartitionRefusalCauseType(vs.Type) {
				continue
			}
			for _, value := range vs.Values {
				lit, ok := value.(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "PartitionRefusalCause constants are string literals")
				unquoted, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				declared[preflight.PartitionRefusalCause(unquoted)] = struct{}{}
			}
		}
	}
	require.NotEmpty(t, declared, "partition.go declares the PartitionRefusalCause constants")

	enumerated := make(map[preflight.PartitionRefusalCause]struct{})
	for _, cause := range preflight.PartitionRefusalCauses() {
		enumerated[cause] = struct{}{}
	}
	assert.Equal(t, declared, enumerated,
		"PartitionRefusalCauses() must enumerate exactly the declared PartitionRefusalCause constants")
}

func isPartitionRefusalCauseType(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "PartitionRefusalCause"
}
