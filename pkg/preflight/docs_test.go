package preflight_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/preflight"
)

// refusalClassesPartitionSection heads the table of docs/refusal-classes.md
// that classifies the partitioned-parent causes.
const refusalClassesPartitionSection = "### `unsupported-partitioned-parent`, keyed on `PartitionRefusalCause`"

// refusalClassesCopySwapSection heads the table that classifies the
// copy-and-swap shape and environment causes.
const refusalClassesCopySwapSection = "### `copy-and-swap-refused`, keyed on `CopySwapRefusalCause`"

// Every partition refusal cause automation can branch on must have a row in
// the refusal-class map: a cause added to the code without a classification
// fails here.
func TestRefusalClassesDocListsEveryPartitionCause(t *testing.T) {
	table := testutil.RefusalCauseTable(t, refusalClassesPartitionSection)
	for _, cause := range preflight.PartitionRefusalCauses() {
		assert.Contains(t, table, testutil.RefusalCauseRow(string(cause)),
			"docs/refusal-classes.md has no class row for partition refusal cause %q", cause)
	}
}

// Every partition refusal cause's Class() is the class its row records: the
// document decides the class, and the code's answer is held to it, so
// neither can be changed without the other.
func TestPartitionRefusalCauseClassMatchesDocClassColumn(t *testing.T) {
	table := testutil.RefusalCauseTable(t, refusalClassesPartitionSection)
	for _, cause := range preflight.PartitionRefusalCauses() {
		assert.Equal(t, testutil.RefusalClassCell(t, table, string(cause)), string(cause.Class()),
			"docs/refusal-classes.md classifies partition refusal cause %q differently from Class()", cause)
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

// Every copy-and-swap shape refusal cause has a row in the refusal-class
// map: a cause added to the code without a classification fails here.
func TestRefusalClassesDocListsEveryCopySwapCause(t *testing.T) {
	table := testutil.RefusalCauseTable(t, refusalClassesCopySwapSection)
	for _, cause := range preflight.CopySwapRefusalCauses() {
		assert.Contains(t, table, testutil.RefusalCauseRow(string(cause)),
			"docs/refusal-classes.md has no class row for copy-and-swap refusal cause %q", cause)
	}
}

// Every copy-and-swap refusal cause's Class() is the class its row records,
// shape and environment causes alike.
func TestCopySwapRefusalCauseClassMatchesDocClassColumn(t *testing.T) {
	table := testutil.RefusalCauseTable(t, refusalClassesCopySwapSection)
	for _, cause := range preflight.CopySwapRefusalCauses() {
		assert.Equal(t, testutil.RefusalClassCell(t, table, string(cause)), string(cause.Class()),
			"docs/refusal-classes.md classifies copy-and-swap refusal cause %q differently from Class()", cause)
	}
}

// The closed set is complete: every CopySwapRefusalCause constant declared
// in copy_swap_shape.go or copy_swap_environment.go is enumerated by
// CopySwapRefusalCauses().
func TestCopySwapRefusalCausesEnumerateEveryDeclaredCause(t *testing.T) {
	declared := declaredStringConstants(t, "copy_swap_shape.go", "CopySwapRefusalCause")
	require.NotEmpty(t, declared, "copy_swap_shape.go declares the shape CopySwapRefusalCause constants")
	environment := declaredStringConstants(t, "copy_swap_environment.go", "CopySwapRefusalCause")
	require.NotEmpty(t, environment, "copy_swap_environment.go declares the environment CopySwapRefusalCause constants")
	for cause := range environment {
		_, duplicate := declared[cause]
		require.False(t, duplicate, "cause %q is declared in both files", cause)
		declared[cause] = struct{}{}
	}

	enumerated := make(map[string]struct{})
	for _, cause := range preflight.CopySwapRefusalCauses() {
		enumerated[string(cause)] = struct{}{}
	}
	assert.Equal(t, declared, enumerated,
		"CopySwapRefusalCauses() must enumerate exactly the declared CopySwapRefusalCause constants")
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
