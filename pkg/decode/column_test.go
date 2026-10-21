package decode

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A present column yields its value, and a NULL — a present column with a
// nil value — is told apart from an absent one by the presence Get returns
// with it, not by the value.
func TestColumnGetTellsNULLFromAbsence(t *testing.T) {
	value, present := PresentColumn("label", "x").Get()
	assert.Equal(t, "x", value)
	assert.True(t, present)

	value, present = PresentColumn("note", nil).Get()
	assert.Nil(t, value)
	assert.True(t, present, "a NULL is a present column")

	value, present = AbsentColumn("blob").Get()
	assert.Nil(t, value)
	assert.False(t, present, "the unchanged-TOAST marker carries no value")
}

// The constructors name the column and decide its presence; nothing else
// about a column is readable.
func TestColumnNameAndPresence(t *testing.T) {
	c := PresentColumn("label", "x")
	assert.Equal(t, "label", c.Name())
	assert.True(t, c.Present())

	c = AbsentColumn("blob")
	assert.Equal(t, "blob", c.Name())
	assert.False(t, c.Present())
}

// A column prints as which of the three it is — a value, a NULL, or the
// unchanged-TOAST marker — and never prints the value, which is row data.
func TestColumnStringTellsTheThreeApartWithoutTheValue(t *testing.T) {
	assert.Equal(t, "label=<value>", PresentColumn("label", "secret").String())
	assert.Equal(t, "note=NULL", PresentColumn("note", nil).String())
	assert.Equal(t, "blob=<absent>", AbsentColumn("blob").String())
	assert.Equal(t, "[label=<value> blob=<absent>]",
		fmt.Sprint([]Column{PresentColumn("label", "secret"), AbsentColumn("blob")}))
}

// Go lets a caller write `value, _ := c.Get()` and take an absent column's
// nil as if it were a NULL, and neither vet nor the lint set flags the
// discard. So no production file under pkg/ may discard the presence of a
// Get call (CO-8): it reads the presence with the value, or does not call
// Get. The check matches every zero-argument Get by name, which is
// stricter than needed and costs nothing while Column's is the only one.
func TestNoProductionCodeDiscardsGetPresence(t *testing.T) {
	var offenders []string
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if discardsGetPresence(n) {
				offenders = append(offenders, fset.Position(n.Pos()).String())
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, offenders)
}

// discardsGetPresence reports whether n is `x, _ := <expr>.Get()` or
// `x, _ = <expr>.Get()`.
func discardsGetPresence(n ast.Node) bool {
	assign, ok := n.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Get" {
		return false
	}
	presence, ok := assign.Lhs[1].(*ast.Ident)
	return ok && presence.Name == "_"
}
