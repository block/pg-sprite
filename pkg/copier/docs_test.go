package copier_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
)

// refusalClassesCopierSection heads the table that classifies the copier
// causes. The shadow table in the same file lists some of the same names
// under its own meaning, so the rows are looked for in this section alone.
const refusalClassesCopierSection = "### Copier refusals, keyed on `copier.RefusalCause`"

// Every copier refusal cause has a row in the copier table of the
// refusal-class map: a cause added to the code without a classification
// fails here.
func TestRefusalClassesDocListsEveryCopierCause(t *testing.T) {
	table := testutil.RefusalCauseTable(t, refusalClassesCopierSection)
	for _, cause := range copier.RefusalCauses() {
		assert.Contains(t, table, testutil.RefusalCauseRow(string(cause)),
			"docs/refusal-classes.md has no copier class row for copier refusal cause %q", cause)
	}
}

// Every copier refusal cause's Class() is the class its row in the copier
// table records: the document decides the class, and the code's answer is
// held to it, so neither can be changed without the other.
func TestRefusalCauseClassMatchesDocClassColumn(t *testing.T) {
	table := testutil.RefusalCauseTable(t, refusalClassesCopierSection)
	for _, cause := range copier.RefusalCauses() {
		assert.Equal(t, testutil.RefusalClassCell(t, table, string(cause)), string(cause.Class()),
			"docs/refusal-classes.md classifies copier refusal cause %q differently from Class()", cause)
	}
}

// The closed set is complete: every RefusalCause constant declared in
// refusal.go is enumerated by RefusalCauses(), so a constant added without
// its entry cannot pass the doc test above unclassified.
func TestRefusalCausesEnumerateEveryDeclaredCause(t *testing.T) {
	declared := declaredStringConstants(t, "refusal.go", "RefusalCause")
	require.NotEmpty(t, declared, "refusal.go declares the RefusalCause constants")

	enumerated := make(map[string]struct{})
	for _, cause := range copier.RefusalCauses() {
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
