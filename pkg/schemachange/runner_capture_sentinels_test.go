package schemachange

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// applierSentinelsNotRunFailures are the applier's exported Err* sentinels
// captureFailed does not list, each with why a stopped catch-up never needs
// it to read as the capture's own failure.
var applierSentinelsNotRunFailures = map[string]string{
	"ErrInvalidCatchupOptions": "refused by NewCatchup, before the capture's goroutine starts",
	"ErrInvalidOptions":        "refused by NewFlusher, before the catch-up exists",
	"ErrCatchupAlreadyRun":     "returned by Run before its first cycle and never wrapped with a stop, so failure reports it as it stands",
	"ErrConversionDrift":       "every flush that raises it wraps ErrInvariantViolation with it, which captureFailed lists",
}

// Every exported Err* sentinel pkg/applier declares is either one
// captureFailed reads through a stop, or one the list above says a stop
// never carries. A new sentinel Catchup.Run can return that is in neither
// fails here, rather than letting a capture that failed as its owner
// stopped it read as a clean stop.
func TestCaptureFailedCoversEveryApplierSentinel(t *testing.T) {
	declared := applierErrSentinels(t)
	listed := captureFailedApplierSentinels(t)

	for _, name := range declared {
		_, exempt := applierSentinelsNotRunFailures[name]
		assert.True(t, slices.Contains(listed, name) || exempt,
			"applier.%s is neither in captureFailed's sentinels nor in applierSentinelsNotRunFailures: decide whether a stopped catch-up can carry it", name)
	}
	for name := range applierSentinelsNotRunFailures {
		assert.Contains(t, declared, name, "applierSentinelsNotRunFailures names applier.%s, which pkg/applier no longer declares", name)
		assert.NotContains(t, listed, name, "applier.%s is both listed by captureFailed and exempted from it", name)
	}
}

// applierErrSentinels walks pkg/applier's non-test source for exported
// package-level variables named Err*.
func applierErrSentinels(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "applier")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	fset := token.NewFileSet()
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			names = append(names, errVarNames(gen)...)
		}
	}
	require.NotEmpty(t, names, "the walker found pkg/applier's sentinels")
	return names
}

// errVarNames is the exported Err* names one var declaration declares.
func errVarNames(gen *ast.GenDecl) []string {
	var names []string
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, ident := range vs.Names {
			if strings.HasPrefix(ident.Name, "Err") && ident.IsExported() {
				names = append(names, ident.Name)
			}
		}
	}
	return names
}

// captureFailedApplierSentinels reads captureFailed's body in
// runner_capture.go for every applier.Err* selector it names.
func captureFailedApplierSentinels(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "runner_capture.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "captureFailed" {
			body = fn.Body
		}
	}
	require.NotNil(t, body, "runner_capture.go declares captureFailed")

	var names []string
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "applier" && strings.HasPrefix(sel.Sel.Name, "Err") {
			names = append(names, sel.Sel.Name)
		}
		return true
	})
	require.NotEmpty(t, names, "captureFailed names applier sentinels")
	return names
}
