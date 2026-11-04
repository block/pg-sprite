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
	callers, packages := skipOnSharedServerCallers(t, root)
	listed, listedPackages := ownServerTargetList(t, root)
	assert.Equal(t, callers, listed,
		"make test-own-server must -run exactly the tests that call testutil.SkipOnSharedServer")
	assert.Equal(t, packages, listedPackages,
		"make test-own-server must name exactly the packages of those tests")
}

// A test decides for itself that it needs its own server only through the
// helper: a raw PG_DSN check in a test file would skip on the shared server
// without appearing in the Makefile's list.
func TestSharedServerIsInspectedOnlyThroughTheHelper(t *testing.T) {
	root := filepath.Join("..", "..")
	// Assembled so that this file does not itself contain the needle.
	inlineCheck := "Getenv(" + strconv.Quote("PG_DSN") + ")"
	var inline []string
	walkTestFiles(t, root, func(path string, body []byte) {
		if strings.Contains(string(body), inlineCheck) {
			inline = append(inline, filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator))))
		}
	})
	assert.Empty(t, inline, "skip on the shared server through testutil.SkipOnSharedServer so make test-own-server runs the test")
}

// skipOnSharedServerCallers returns the sorted names of the test functions
// whose body calls SkipOnSharedServer, and the sorted ./-relative packages
// that declare them.
func skipOnSharedServerCallers(t *testing.T, root string) (names, packages []string) {
	t.Helper()
	seenPackage := make(map[string]struct{})
	walkTestFiles(t, root, func(path string, body []byte) {
		parsed, err := parser.ParseFile(token.NewFileSet(), path, body, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Body == nil {
				continue
			}
			if !callsSkipOnSharedServer(fn.Body) {
				continue
			}
			names = append(names, fn.Name.Name)
			rel, err := filepath.Rel(root, filepath.Dir(path))
			require.NoError(t, err)
			seenPackage["./"+filepath.ToSlash(rel)] = struct{}{}
		}
	})
	for pkg := range seenPackage {
		packages = append(packages, pkg)
	}
	sort.Strings(names)
	sort.Strings(packages)
	return names, packages
}

// callsSkipOnSharedServer reports whether body contains a call whose callee
// is named SkipOnSharedServer, qualified by the testutil package or not.
func callsSkipOnSharedServer(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
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
