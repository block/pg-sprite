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

// captureSentinelsNotRunFailures are the exported Err* sentinels and *Error
// types of the packages a running catch-up returns from (its own and the
// decoding package its stream and slot checks come from) that captureFailed
// does not name, each with why a stopped catch-up never needs it to read as
// the capture's own failure.
var captureSentinelsNotRunFailures = map[string]map[string]string{
	"applier": {
		"ErrInvalidCatchupOptions": "refused by NewCatchup, before the capture's goroutine starts",
		"ErrInvalidOptions":        "refused by NewFlusher, before the catch-up exists",
		"ErrCatchupAlreadyRun":     "returned by Run before its first cycle and never wrapped with a stop, so failure reports it as it stands",
		"ErrConversionDrift":       "every flush that raises it wraps ErrInvariantViolation with it, which captureFailed lists",
	},
	"decode": {
		"ErrInvariantViolation":     "the same value as applier.ErrInvariantViolation (both are dbconn's), which captureFailed lists",
		"ErrSlotActive":             "returned only by DropSlot, which runs after the capture has ended",
		"ErrForeignDecodingState":   "raised while the route creates or adopts its slot and publication, before the capture starts",
		"ForeignStateError":         "raised while the route creates or adopts its slot and publication, before the capture starts",
		"PublicationPrivilegeError": "raised while the route creates its publication, before the capture starts",
		"SlotExistsError":           "raised by CreateSlot, before the capture starts",
	},
}

// Every exported Err* sentinel and *Error type pkg/applier and pkg/decode
// declare is either one captureFailed reads through a stop, or one the
// list above says a stop never carries. A new one Catchup.Run can return
// that is in neither fails here, rather than letting a capture that failed
// as its owner stopped it read as a clean stop.
func TestCaptureFailedCoversEveryCaptureSentinel(t *testing.T) {
	listed := captureFailedSelectors(t)
	for pkg, exempt := range captureSentinelsNotRunFailures {
		t.Run(pkg, func(t *testing.T) {
			declared := errorDeclarations(t, pkg)
			for _, name := range declared {
				_, isExempt := exempt[name]
				assert.True(t, slices.Contains(listed[pkg], name) || isExempt,
					"%s.%s is neither in captureFailed nor in captureSentinelsNotRunFailures: decide whether a stopped catch-up can carry it", pkg, name)
			}
			for name := range exempt {
				assert.Contains(t, declared, name, "captureSentinelsNotRunFailures names %s.%s, which pkg/%s no longer declares", pkg, name, pkg)
				assert.NotContains(t, listed[pkg], name, "%s.%s is both named by captureFailed and exempted from it", pkg, name)
			}
		})
	}
}

// errorDeclarations walks a sibling package's non-test source for its
// exported package-level Err* variables and *Error types.
func errorDeclarations(t *testing.T, pkg string) []string {
	t.Helper()
	dir := filepath.Join("..", pkg)
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
			if !ok {
				continue
			}
			names = append(names, errorNames(gen)...)
		}
	}
	require.NotEmpty(t, names, "the walker found pkg/%s's sentinels", pkg)
	return names
}

// errorNames is the exported Err* variables or *Error types one
// declaration declares.
func errorNames(gen *ast.GenDecl) []string {
	var names []string
	for _, spec := range gen.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			if gen.Tok != token.VAR {
				continue
			}
			for _, ident := range s.Names {
				if strings.HasPrefix(ident.Name, "Err") && ident.IsExported() {
					names = append(names, ident.Name)
				}
			}
		case *ast.TypeSpec:
			if strings.HasSuffix(s.Name.Name, "Error") && s.Name.IsExported() {
				names = append(names, s.Name.Name)
			}
		}
	}
	return names
}

// captureFailedSelectors reads captureFailed's body in runner_capture.go
// for every Err* sentinel and *Error type it names, by package.
func captureFailedSelectors(t *testing.T) map[string][]string {
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

	names := map[string][]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		name := sel.Sel.Name
		if ok && (strings.HasPrefix(name, "Err") || strings.HasSuffix(name, "Error")) {
			names[pkg.Name] = append(names[pkg.Name], name)
		}
		return true
	})
	require.NotEmpty(t, names["applier"], "captureFailed names applier sentinels")
	require.NotEmpty(t, names["decode"], "captureFailed names decode sentinels")
	return names
}
