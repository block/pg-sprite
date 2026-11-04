package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ownServerTarget is the Makefile target that runs the tests which skip on
// the shared server, and ownServerRecipe is the shape of its recipe: the
// -run anchor lists the test names, and the arguments after it are the
// packages they live in.
const ownServerTarget = "test-own-server:"

var ownServerRecipe = regexp.MustCompile(`-run '\^\(([^)]*)\)\$\$' (.*)$`)

// Every test that calls SkipOnSharedServer is run by make test-own-server,
// and nothing else is: the long-lived-database targets skip these tests, so
// a caller added without a row in the Makefile would run on no gate at all,
// and a stale row would hide a renamed test behind "no tests to run".
func TestOwnServerTargetRunsEveryTestThatSkipsOnTheSharedServer(t *testing.T) {
	root := filepath.Join("..", "..")
	callers, packages, indirect := skipOnSharedServerCallers(t, root)
	require.Empty(t, indirect,
		"call testutil.SkipOnSharedServer directly from the Test function that needs its own server, not from a helper: the Makefile lists tests by name, and a test that skips through a helper is not listed")
	listed, listedPackages := ownServerTargetList(t, root)
	assert.Equal(t, callers, listed,
		"make test-own-server must -run exactly the tests that call testutil.SkipOnSharedServer")
	assert.Equal(t, packages, listedPackages,
		"make test-own-server must name exactly the packages of those tests")
}

// A test decides for itself that it needs its own server only through the
// helper: a test file that reads the shared server's variable, however it
// reads it, could skip on the shared server without appearing in the
// Makefile's list.
func TestSharedServerIsInspectedOnlyThroughTheHelper(t *testing.T) {
	root := filepath.Join("..", "..")
	var inline []string
	walkTestFiles(t, root, func(path string, body []byte) {
		parsed, err := parser.ParseFile(token.NewFileSet(), path, body, parser.SkipObjectResolution)
		require.NoError(t, err)
		if namesSharedServerVariable(parsed) {
			inline = append(inline, filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator))))
		}
	})
	assert.Empty(t, inline, "skip on the shared server through testutil.SkipOnSharedServer so make test-own-server runs the test")
}

// sharedServerVariable is the environment variable that points the suite at
// a shared server, assembled so that this file's own literals do not name it.
var sharedServerVariable = strconv.Quote("PG_" + "DSN")

// namesSharedServerVariable reports whether any string literal in file is
// the shared server's variable name. A literal, not a call shape, is the
// needle: os.Getenv, os.LookupEnv, and any other way of reading the
// environment all have to spell the name.
func namesSharedServerVariable(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING && lit.Value == sharedServerVariable {
			found = true
		}
		return !found
	})
	return found
}

// skipOnSharedServerCallers returns the sorted names of the top-level Test
// functions whose body calls SkipOnSharedServer, the sorted ./-relative
// packages that declare them, and every other declaration in a test file
// that calls it — a helper, a method, or a package-level function literal
// — as file:name. Only a top-level Test function is a name the Makefile
// can run, so any other caller skips tests the target cannot list.
func skipOnSharedServerCallers(t *testing.T, root string) (names, packages, indirect []string) {
	t.Helper()
	seenPackage := make(map[string]struct{})
	walkTestFiles(t, root, func(path string, body []byte) {
		parsed, err := parser.ParseFile(token.NewFileSet(), path, body, parser.SkipObjectResolution)
		require.NoError(t, err)
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		for _, decl := range parsed.Decls {
			if !callsSkipOnSharedServer(decl) {
				continue
			}
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				indirect = append(indirect, filepath.ToSlash(rel)+":"+declName(decl))
				continue
			}
			names = append(names, fn.Name.Name)
			seenPackage["./"+filepath.ToSlash(filepath.Dir(rel))] = struct{}{}
		}
	})
	for pkg := range seenPackage {
		packages = append(packages, pkg)
	}
	sort.Strings(names)
	sort.Strings(packages)
	sort.Strings(indirect)
	return names, packages, indirect
}

// declName names a declaration for a failure message: a function or method
// by its name, anything else by its first declared identifier.
func declName(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return d.Name.Name
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			if v, ok := spec.(*ast.ValueSpec); ok && len(v.Names) > 0 {
				return v.Names[0].Name
			}
		}
	}
	return "declaration"
}

// callsSkipOnSharedServer reports whether decl contains a call whose callee
// is named SkipOnSharedServer, qualified by the testutil package or not.
func callsSkipOnSharedServer(decl ast.Decl) bool {
	found := false
	ast.Inspect(decl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch callee := call.Fun.(type) {
		case *ast.SelectorExpr:
			found = found || callee.Sel.Name == "SkipOnSharedServer"
		case *ast.Ident:
			found = found || callee.Name == "SkipOnSharedServer"
		}
		return !found
	})
	return found
}

// ownServerTargetList reads the Makefile's test-own-server recipe and returns
// the sorted test names in its -run anchor and the sorted packages after it.
func ownServerTargetList(t *testing.T, root string) (names, packages []string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "Makefile"))
	require.NoError(t, err)
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		if line != ownServerTarget {
			continue
		}
		require.Less(t, i+1, len(lines), "the %s target has a recipe", ownServerTarget)
		m := ownServerRecipe.FindStringSubmatch(lines[i+1])
		require.NotNil(t, m, "the %s recipe anchors -run on '^(...)$$' and lists its packages after it", ownServerTarget)
		names = strings.Split(m[1], "|")
		packages = strings.Fields(m[2])
		sort.Strings(names)
		sort.Strings(packages)
		return names, packages
	}
	require.Failf(t, "missing target", "the Makefile has no %s target", ownServerTarget)
	return nil, nil
}

// walkTestFiles calls visit with every _test.go file under root.
func walkTestFiles(t *testing.T, root string, visit func(path string, body []byte)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		visit(path, body)
		return nil
	})
	require.NoError(t, err)
}
