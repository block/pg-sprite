package executor_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

// executionModelDoc is the human-facing contract page this test keeps honest.
const executionModelDoc = "../../docs/execution-model.md"

// Every step kind automation can branch on must be named in the execution
// model: a StepKind added to the code without the doc naming it fails here.
func TestDocNamesEveryStepKind(t *testing.T) {
	raw, err := os.ReadFile(executionModelDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, k := range executor.StepKinds() {
		assert.Contains(t, doc, fmt.Sprintf("`%s`", k),
			"docs/execution-model.md does not name step kind %q", k)
	}
}

// Every outcome code automation can branch on must be named in the
// execution model: a Code added to the vocabulary without the doc naming
// it fails here.
func TestDocNamesEveryOutcomeCode(t *testing.T) {
	raw, err := os.ReadFile(executionModelDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, c := range executor.Codes() {
		assert.Contains(t, doc, fmt.Sprintf("`%s`", c),
			"docs/execution-model.md does not name outcome code %q", c)
	}
}

// The doc's Permanent column is Code.Permanent(): an adapter author reading
// the table and one calling the method must reach the same retry class for
// every code.
func TestDocPermanentColumnMatchesCodePermanent(t *testing.T) {
	raw, err := os.ReadFile(executionModelDoc)
	require.NoError(t, err)
	rows := make(map[executor.Code]string)
	for line := range strings.SplitSeq(string(raw), "\n") {
		for _, c := range executor.Codes() {
			if strings.HasPrefix(line, fmt.Sprintf("| `%s` | ", c)) {
				rows[c] = line
			}
		}
	}
	for _, c := range executor.Codes() {
		row, ok := rows[c]
		require.True(t, ok, "docs/execution-model.md has no outcome-code table row for %q", c)
		want := "| no |"
		if c.Permanent() {
			want = "| yes |"
		}
		assert.Contains(t, row, want, "the Permanent column for %q disagrees with Code.Permanent()", c)
	}
}

// The closed set is complete: every Code constant declared in the package
// is enumerated by Codes(). A constant added without its Codes() entry would
// pass the doc test above (which walks Codes()) while adapters enumerating
// Codes() never learned to render it.
func TestCodesEnumerateEveryDeclaredCode(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "code.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	declared := make(map[executor.Code]struct{})
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || !isCodeType(vs.Type) {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "Code constants are string literals")
				value, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				declared[executor.Code(value)] = struct{}{}
			}
		}
	}
	require.NotEmpty(t, declared, "code.go declares the Code constants")

	enumerated := make(map[executor.Code]struct{})
	for _, c := range executor.Codes() {
		enumerated[c] = struct{}{}
	}
	assert.Equal(t, declared, enumerated, "Codes() must enumerate exactly the declared Code constants")
}

func isCodeType(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "Code"
}

// The closed set has no duplicates: a code pasted twice would silently
// shadow a missing entry.
func TestCodesAreUnique(t *testing.T) {
	seen := make(map[executor.Code]struct{})
	for _, c := range executor.Codes() {
		_, dup := seen[c]
		assert.False(t, dup, "duplicate outcome code %q", c)
		seen[c] = struct{}{}
	}
}
