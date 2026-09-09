package safety

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// proofTypeRegistries are the hand-maintained prose lists of proof types.
// They drift independently, so each must name every proof type the code
// defines.
var proofTypeRegistries = []string{
	"../../SAFETY.md",
	"../../.agents/checks/review.md",
	"../../docs/tcb-model.md",
}

// sentinelProofTypes must be among the derived set: a walker that finds
// nothing, or that stops recognising the established shape, fails here
// rather than passing vacuously.
var sentinelProofTypes = []string{"PreflightedTable", "VerifiedShadow", "TableLock"}

// A proof type is an exported struct whose every field is unexported and
// whose doc comment opens "<Name> proves": the shape SAFETY.md prescribes
// for a value only its validating passage can mint.
func isProofType(spec *ast.TypeSpec, doc *ast.CommentGroup) bool {
	if !spec.Name.IsExported() {
		return false
	}
	st, ok := spec.Type.(*ast.StructType)
	if !ok || st.Fields == nil || len(st.Fields.List) == 0 {
		return false
	}
	for _, field := range st.Fields.List {
		for _, name := range field.Names {
			if name.IsExported() {
				return false
			}
		}
	}
	if doc == nil {
		return false
	}
	return strings.HasPrefix(doc.Text(), spec.Name.Name+" proves ")
}

// inventory is what the walk learns about the code under one root: the
// proof types it defines and, per package name, every exported top-level
// type and function — the identifiers prose may legitimately name.
type inventory struct {
	proofTypes []string
	declared   map[string]map[string]bool
}

// inventoryOf walks every non-test Go file under root.
func inventoryOf(t *testing.T, root string) inventory {
	t.Helper()
	fset := token.NewFileSet()
	inv := inventory{declared: map[string]map[string]bool{}}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		inv.record(file)
		return nil
	})
	require.NoError(t, err)
	sort.Strings(inv.proofTypes)
	return inv
}

func (inv *inventory) record(file *ast.File) {
	pkg := file.Name.Name
	if inv.declared[pkg] == nil {
		inv.declared[pkg] = map[string]bool{}
	}
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Recv == nil && decl.Name.IsExported() {
				inv.declared[pkg][decl.Name.Name] = true
			}
		case *ast.GenDecl:
			if decl.Tok != token.TYPE {
				continue
			}
			for _, spec := range decl.Specs {
				ts := spec.(*ast.TypeSpec)
				if ts.Name.IsExported() {
					inv.declared[pkg][ts.Name.Name] = true
				}
				doc := ts.Doc
				if doc == nil {
					doc = decl.Doc
				}
				if isProofType(ts, doc) {
					inv.proofTypes = append(inv.proofTypes, pkg+"."+ts.Name.Name)
				}
			}
		}
	}
}

// namesProofType reports whether prose names the type in code font, either
// bare (`Name`) or package-qualified (`pkg.Name`), so a mention inside a
// longer identifier does not count.
func namesProofType(raw, name string) bool {
	return strings.Contains(raw, "`"+name+"`") || strings.Contains(raw, "."+name+"`")
}

// qualifiedMention matches a code-font `pkg.Name` where pkg is a Go package
// name and Name an exported identifier; `pkg.file.go` and `Type.Method` do
// not match.
var qualifiedMention = regexp.MustCompile("`([a-z][a-z0-9]*)\\.([A-Z][A-Za-z0-9_]*)`")

// staleMentions returns every qualified mention in prose whose package the
// inventory knows but which names nothing that package exports. Mentions
// of packages outside the inventory (stdlib, pgx) are not its concern.
func (inv inventory) staleMentions(prose string) []string {
	var stale []string
	for _, m := range qualifiedMention.FindAllStringSubmatch(prose, -1) {
		pkg, name := m[1], m[2]
		exported, known := inv.declared[pkg]
		if known && !exported[name] {
			stale = append(stale, pkg+"."+name)
		}
	}
	return stale
}

// Every proof type defined under pkg/ must be named in every registry, so a
// proof type added or renamed without updating all three lists fails here.
// The reverse holds for qualified names: a registry may not name a
// `pkg.Name` that the package does not export, so a proof type dropped or
// renamed in the code while a registry still lists it fails here too.
// Neither list is maintained by hand in a test.
func TestRegistriesNameEveryProofType(t *testing.T) {
	inv := inventoryOf(t, "../../pkg")
	bare := make([]string, 0, len(inv.proofTypes))
	for _, qualified := range inv.proofTypes {
		bare = append(bare, qualified[strings.LastIndex(qualified, ".")+1:])
	}
	for _, sentinel := range sentinelProofTypes {
		require.Contains(t, bare, sentinel, "the proof-type walker no longer recognises %s; derived set: %v", sentinel, inv.proofTypes)
	}
	for _, registry := range proofTypeRegistries {
		raw, err := os.ReadFile(registry)
		require.NoError(t, err)
		for _, name := range bare {
			assert.True(t, namesProofType(string(raw), name), "%s does not name the proof type %s", registry, name)
		}
		assert.Empty(t, inv.staleMentions(string(raw)), "%s names identifiers its package does not export", registry)
	}
}

// The reverse direction must see through the shapes prose actually uses: a
// real qualified type resolves, a qualified name the package no longer
// exports is stale, and mentions of foreign packages, file names, and
// Type.Method pairs are neither.
func TestStaleMentions(t *testing.T) {
	inv := inventoryOf(t, "../../pkg")
	prose := "`statement.Statement`, `statement.Classified`, `time.Sleep`, `runner.go`, `Tracker.CancelBuild`"
	assert.Equal(t, []string{"statement.Classified"}, inv.staleMentions(prose))
}
