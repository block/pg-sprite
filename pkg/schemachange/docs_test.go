package schemachange_test

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

	"github.com/block/pg-sprite/pkg/schemachange"
)

// refusalClassesDoc is the routing contract that classifies every shadow
// refusal cause; these tests keep its cause table honest.
const refusalClassesDoc = "../../docs/refusal-classes.md"

// Every shadow refusal cause has a row in the refusal-class map: a cause
// added to the code without a classification fails here.
func TestRefusalClassesDocListsEveryShadowCause(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, cause := range schemachange.RefusalCauses() {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", cause),
			"docs/refusal-classes.md has no class row for shadow refusal cause %q", cause)
	}
}

// The closed set is complete: every RefusalCause constant declared in
// refusal.go is enumerated by RefusalCauses(), so a constant added without
// its entry cannot pass the doc test above unclassified.
func TestRefusalCausesEnumerateEveryDeclaredCause(t *testing.T) {
	declared := declaredStringConstants(t, "refusal.go", "RefusalCause")
	require.NotEmpty(t, declared, "refusal.go declares the RefusalCause constants")

	enumerated := make(map[string]struct{})
	for _, cause := range schemachange.RefusalCauses() {
		enumerated[string(cause)] = struct{}{}
	}
	assert.Equal(t, declared, enumerated,
		"RefusalCauses() must enumerate exactly the declared RefusalCause constants")
}

// declaredStringConstants collects the string literals of every constant of
// the named type declared in file.
func declaredStringConstants(t *testing.T, file, typeName string) map[string]struct{} {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	declared := make(map[string]struct{})
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != typeName {
				continue
			}
			for _, value := range vs.Values {
				lit, ok := value.(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "%s constants are string literals", typeName)
				unquoted, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				declared[unquoted] = struct{}{}
			}
		}
	}
	return declared
}
