package safety

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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

// proofTypes walks every non-test Go file under root and returns the
// qualified proof types it defines, sorted.
func proofTypes(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var found []string
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
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				doc := ts.Doc
				if doc == nil {
					doc = gen.Doc
				}
				if isProofType(ts, doc) {
					found = append(found, file.Name.Name+"."+ts.Name.Name)
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(found)
	return found
}

// namesProofType reports whether prose names the type in code font, either
// bare (`Name`) or package-qualified (`pkg.Name`), so a mention inside a
// longer identifier does not count.
func namesProofType(raw, name string) bool {
	return strings.Contains(raw, "`"+name+"`") || strings.Contains(raw, "."+name+"`")
}

// Every proof type defined under pkg/ must be named in every registry: a
// proof type added, renamed, or dropped without updating all three lists
// fails here, and no list has to be maintained by hand in a test.
func TestRegistriesNameEveryProofType(t *testing.T) {
	derived := proofTypes(t, "../../pkg")
	bare := make([]string, 0, len(derived))
	for _, qualified := range derived {
		bare = append(bare, qualified[strings.LastIndex(qualified, ".")+1:])
	}
	for _, sentinel := range sentinelProofTypes {
		require.Contains(t, bare, sentinel, "the proof-type walker no longer recognises %s; derived set: %v", sentinel, derived)
	}
	for _, registry := range proofTypeRegistries {
		raw, err := os.ReadFile(registry)
		require.NoError(t, err)
		for _, name := range bare {
			assert.True(t, namesProofType(string(raw), name), "%s does not name the proof type %s", registry, name)
		}
	}
}
